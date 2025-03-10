package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/stealer"
	"hirocrossclusterworkloads/pkg/metrics"

	nats "github.com/nats-io/nats.go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Consume struct {
	StealerConfig stealer.SWConfig
	K8SCli        *kubernetes.Clientset
	K8SConfig     *rest.Config
}

func New(stealerConfig stealer.SWConfig) (stealer.Stealer, error) {
	clientset, config, err := common.GetK8sClientAndConfigSet()
	if err != nil {
		return nil, err
	}
	//Consume implements Stealer interface
	return &Consume{
		K8SCli:        clientset,
		StealerConfig: stealerConfig,
		K8SConfig:     config,
	}, nil
}

func (c *Consume) Start(stopChan chan<- bool) error {
	defer func() { stopChan <- true }()

	natsConnect, js, kv, err := c.StealerConfig.Nclient.GetNATSConnectJetStreamAndKeyValue()
	if err != nil {
		slog.Error("Failed to connect to NATS/JS/KV", "error", err, "natsConnect", natsConnect, "js", js, "kv", kv)
		return err
	}
	defer natsConnect.Close()

	jetStreamName := c.StealerConfig.Nclient.GetJetStreamName()
	jetStreamQueue := c.StealerConfig.Nclient.GetJetStreamQueueName()
	slog.Info("Subscribe to Pod stealing messages...", "stream", jetStreamName,
		"queue", jetStreamQueue, "subject", c.StealerConfig.Nclient.NATSSubject)

	// Queue Group ensures only one consumer gets a message
	js.QueueSubscribe(c.StealerConfig.Nclient.NATSSubject, jetStreamQueue, func(msg *nats.Msg) {
		c.InputMsgHandler(msg, kv)
	})
	select {}
}

func (c *Consume) InputMsgHandler(msg *nats.Msg, kv nats.KeyValue) {
	var pod corev1.Pod
	var donorPod common.DonorPod
	var donorUUID string
	var kvKey string
	var waitTime int
	var stealerUUID = c.StealerConfig.StealerUUID
	slog.Info("Received message", "subject", c.StealerConfig.Nclient.NATSSubject, "data", string(msg.Data))

	// Deserialize the entire donotPodMap metadata to JSON
	err := json.Unmarshal(msg.Data, &donorPod)
	if err != nil {
		slog.Error("Failed to Unmarshal donorPodMap from rawData",
			"error", err, "rawData", string(msg.Data))
		msg.Nak()
		return
	}
	pod = *donorPod.Pod
	slog.Info("Deserialized Pod", "pod", pod)
	// Check if the Pod already exists
	_, err = c.K8SCli.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err == nil {
		slog.Info("Pod already exists, skipping",
			"podName", pod.Name, "podNamespace", pod.Namespace)
		msg.Nak()
		return
	} else if !apierrors.IsNotFound(err) {
		slog.Error("Failed to check if Pod exists", "podName", pod.Name,
			"podNamespace", pod.Namespace, "error", err)
		msg.Nak()
		return
	}
	donorUUID = donorPod.DonorDetails.DonorUUID
	kvKey = donorPod.DonorDetails.KVKey
	waitTime = donorPod.DonorDetails.WaitTime
	slog.Info("Deserialized", "donorUUID", donorUUID, "kvKey", kvKey, "waitTime", waitTime)

	// Check if message is already processed
	entry, err := kv.Get(kvKey)
	if err != nil {
		slog.Error("Failed to get value from KV bucket", "error", err)
		msg.Nak()
		return

	}
	servingStealerDetails := common.StealerDetails{}
	err = json.Unmarshal(entry.Value(), &servingStealerDetails)
	if err != nil {
		slog.Error("Failed to unmarshal servingStealerDetails", "error", err)
		msg.Nak()
		return
	}
	otherStealerUUID := servingStealerDetails.StealerUUID
	if otherStealerUUID == common.DonorKVValuePending {
		slog.Info("I can process the Pod", "podName", pod.Name,
			"podNamespace", pod.Namespace, "stealerUUID", stealerUUID)
	} else if otherStealerUUID == stealerUUID {
		slog.Info("Skipping Pod, I am already processed it", "podName", pod.Name,
			"podNamespace", pod.Namespace, "stealerUUID", stealerUUID)
		StealPod(c.K8SCli, pod, donorUUID, stealerUUID)
		msg.Ack()
		return
	} else {
		slog.Info("Skipping Pod, already processed by another stealer", "podName", pod.Name,
			"podNamespace", pod.Namespace, "otherStealerUUID", otherStealerUUID)
		return
	}

	servingStealerDetails.StealerUUID = stealerUUID
	servingStealerDetails.KVKey = kvKey

	servingStealerDetailsBytes, err := json.Marshal(servingStealerDetails)
	if err != nil {
		slog.Error("Failed to marshal servingStealerDetails", "error", err)
		msg.Nak()
	}
	// Mark with stealerUUID in KV by this stealer
	_, err = kv.Update(kvKey, servingStealerDetailsBytes, entry.Revision())
	if err != nil && !errors.Is(err, nats.ErrKeyExists) {
		slog.Error("Failed to update value in KV bucket with stealerUUID: ", "error", err)
		msg.Nak()
	}

	// Acknowledge JetStream message
	msg.Ack()

	createdPod, err := StealPod(c.K8SCli, pod, donorUUID, stealerUUID)
	if err != nil {
		slog.Error("Failed to steal Pod", "error", err)
		msg.Nak()
	}
	slog.Info("Successfully stole the Pod", "pod", createdPod)

	metrics.StealerClaimedTasksTotal.WithLabelValues(stealerUUID).Inc()
	metrics.StolenTasksTotal.WithLabelValues(donorUUID, stealerUUID).Inc()

	// Watch the created pod for every 5 sec and send it status to donor
	// go func() {
	// 	pollKVKey := common.GeneratePollStealWorkloadKVKey(donorUUID, stealerUUID, pod.Namespace, pod.Name)
	// 	checkForAnyFailuresOrRestarts(c.K8SCli, createdPod, kv, pollKVKey, waitTime)
	// }()

	fqdns, err := ExposeServiceAndExportViaSubmariner(c.K8SCli, c.K8SConfig, createdPod, donorUUID, stealerUUID)
	if err != nil {
		slog.Error("Failed to expose Service and export via submariner", "error", err)
		return
	}
	slog.Info("Successfully exposed Service and exported via submariner", "fqdns", fqdns)

	servingStealerDetails.ExposedFQDNs = fqdns
	servingStealerDetailsBytes, err = json.Marshal(servingStealerDetails)
	if err != nil {
		slog.Error("Failed to marshal servingStealerDetails", "error", err)
		return
	}
	// Mark with servingStealerDetails in KV by this stealer
	for retries := 3; retries > 0; retries-- {
		_, err = kv.Update(kvKey, servingStealerDetailsBytes, entry.Revision())
		if err == nil {
			slog.Info("Successfully updated the KV bucket with exposed FQDNs", "fqdns", fqdns)
			break
		}
		if errors.Is(err, nats.ErrKeyExists) {
			entry, err = kv.Get(kvKey)
			if err != nil {
				slog.Error("Failed to get updated value from KV bucket", "error", err)
				return
			}
			continue
		}
		slog.Error("Failed to update value in KV bucket with FQDNs: ", "error", err)
		return
	}
	slog.Info("Successfully updated the KV bucket with exposed FQDNs", "fqdns", fqdns)
}

func StealPod(cli *kubernetes.Clientset, pod corev1.Pod, donorUUID string, stealerUUID string) (*corev1.Pod, error) {
	success, err := CreateNamespace(cli, pod.Namespace)
	if !success || err != nil {
		slog.Error("Error occurred", "error", err)
		return nil, err
	}

	sterilizeResourceInplace(&pod, donorUUID, stealerUUID)

	// Get the Pod with the specified name and namespace
	existingPod, err := cli.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err == nil {
		if common.AreMapsEqual(existingPod.Labels, pod.Labels) {
			slog.Info("Pod already stolen", "podName", pod.Name, "podNamespace", pod.Namespace)
			return existingPod, nil
		} else {
			slog.Error("Cannot Steal; Same Pod exists with the specified details", "podName", pod.Name, "podNamespace", pod.Namespace)
			return nil, fmt.Errorf("cannot steal; same pod already exists with the specified details: podName=%s, podNamespace=%s", pod.Name, pod.Namespace)
		}
	} else if !apierrors.IsNotFound(err) {
		slog.Error("Failed to get Pod with specified details", "podName", pod.Name, "podNamespace", pod.Namespace, "error", err)
		return nil, err
	}

	// Create the Pod in Kubernetes
	createdPod, err := cli.CoreV1().Pods(pod.Namespace).Create(context.TODO(), &pod, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create Pod", "error", err)
		return nil, err
	}

	if !isResourceSuccessfullyRunning(cli, createdPod.Namespace, createdPod.Name, "Pod", 5) {
		slog.Error("Pod is not running within 5 min", "pod", createdPod)
		return nil, fmt.Errorf("pod is not running within 5 min: podName=%s, podNamespace=%s", createdPod.Name, createdPod.Namespace)
	}
	slog.Info("Successfully stole the workload", "pod", createdPod)
	return createdPod, nil
}

func StealDeployment(cli *kubernetes.Clientset, deployment appsv1.Deployment, donorUUID string, stealerUUID string) (*appsv1.Deployment, error) {
	success, err := CreateNamespace(cli, deployment.Namespace)
	if !success || err != nil {
		slog.Error("Error occurred", "error", err)
		return nil, err
	}

	sterilizeResourceInplace(&deployment, donorUUID, stealerUUID)

	// Get the Pod with the specified name and namespace
	existingDeployment, err := cli.AppsV1().Deployments(deployment.Namespace).Get(context.TODO(), deployment.Name, metav1.GetOptions{})
	if err == nil {
		if common.AreMapsEqual(existingDeployment.Labels, deployment.Labels) {
			slog.Info("Deployment already stolen", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)
			return existingDeployment, nil
		} else {
			slog.Error("Cannot Steal; Same Deployment exists with the specified details", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace)
			return nil, fmt.Errorf("cannot steal; same deployment already exists with the specified details: deploymentName=%s, deploymentNamespace=%s", deployment.Name, deployment.Namespace)
		}
	} else if !apierrors.IsNotFound(err) {
		slog.Error("Failed to get Deployment with specified details", "deploymentName", deployment.Name, "deploymentNamespace", deployment.Namespace, "error", err)
		return nil, err
	}

	// Create the Deployment in Kubernetes
	createdDeployment, err := cli.AppsV1().Deployments(deployment.Namespace).Create(context.TODO(), &deployment, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create Deployment", "error", err)
		return nil, err
	}

	if !isResourceSuccessfullyRunning(cli, createdDeployment.Namespace, createdDeployment.Name, "Deployment", 5) {
		slog.Error("Pod is not running within 5 min", "pod", createdDeployment)
		return nil, fmt.Errorf("pod is not running within 5 min: deploymentName=%s, deploymentNamespace=%s", createdDeployment.Name, createdDeployment.Namespace)
	}
	slog.Info("Successfully stole the workload", "pod", createdDeployment)
	return createdDeployment, nil
}

func CreateNamespace(cli *kubernetes.Clientset, namespace string) (bool, error) {
	// Ensure Namespace exists before creating the Pod
	_, err := cli.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace does not exist, create it
			slog.Info("Namespace not found, creating it", "namespace", namespace)
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			}

			_, err := cli.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{})
			if err != nil {
				slog.Error("Failed to create namespace", "namespace", namespace, "error", err)
				return false, err
			}
		} else {
			// Other errors (e.g., API failure)
			slog.Error("Failed to check namespace existence", "namespace", namespace, "error", err)
			return false, err
		}
	}
	return true, nil
}

func sterilizeResourceInplace(resource metav1.Object, donorUUID string, stealerUUID string) {
	switch resource := resource.(type) {
	case *corev1.Pod:
		sterilizePodInplace(resource, donorUUID, stealerUUID)
	case *appsv1.Deployment:
		sterilizeDeploymentInplace(resource, donorUUID, stealerUUID)
	}
}

func sterilizePodInplace(pod *corev1.Pod, donorUUID string, stealerUUID string) {
	newPodObjectMeta := metav1.ObjectMeta{
		Name:        pod.Name,
		Namespace:   pod.Namespace,
		Labels:      fetchStolenLabelsMap(pod, donorUUID, stealerUUID),
		Annotations: pod.Annotations,
	}
	pod.ObjectMeta = newPodObjectMeta
}

func sterilizeDeploymentInplace(deployment *appsv1.Deployment, donorUUID string, stealerUUID string) {
	newDeploymentObjectMeta := metav1.ObjectMeta{
		Name:        deployment.Name,
		Namespace:   deployment.Namespace,
		Labels:      fetchStolenLabelsMap(deployment, donorUUID, stealerUUID),
		Annotations: deployment.Annotations,
	}
	deployment.ObjectMeta = newDeploymentObjectMeta
}

func fetchStolenLabelsMap(resource metav1.Object, donorUUID string, stealerUUID string) map[string]string {
	common.StolenPodLablesMap["donorUUID"] = donorUUID
	common.StolenPodLablesMap["stealerUUID"] = stealerUUID
	return common.MergeMaps(resource.GetLabels(), common.StolenPodLablesMap)
}

func isResourceSuccessfullyRunning(clientset *kubernetes.Clientset, namespace, name string, resourceType string, waitTime int) bool {
	timeout := time.After(time.Duration(waitTime) * time.Minute)    // Timeout after 5 minutes
	ticker := time.NewTicker(time.Duration(waitTime) * time.Second) // Poll every 5 seconds
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			slog.Error("Timeout waiting for resource to reach Running state", "namespace", namespace, "name", name, "resourceType", resourceType)
			return false
		case <-ticker.C:
			switch resourceType {
			case "Pod":
				pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
				if err != nil {
					slog.Error("Failed to get Pod status", "namespace", namespace, "name", name, "error", err)
					return false
				}
				slog.Info("Pod status", "namespace", namespace, "name", name, "phase", pod.Status.Phase)
				if pod.Status.Phase == corev1.PodRunning {
					slog.Info("Pod is now Running", "namespace", namespace, "name", name)
					return true
				}
			case "Deployment":
				deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
				if err != nil {
					slog.Error("Failed to get Deployment status", "namespace", namespace, "name", name, "error", err)
					return false
				}
				slog.Info("Deployment status", "namespace", namespace, "name", name, "conditions", deployment.Status.Conditions)
				for _, condition := range deployment.Status.Conditions {
					if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
						slog.Info("Deployment is now Available", "namespace", namespace, "name", name)
						return true
					}
				}
			}
		}
	}
}

func ExposeServiceAndExportViaSubmariner(K8SCli *kubernetes.Clientset, K8SConfig *rest.Config,
	createdPod *corev1.Pod, donorUUID string, stealerUUID string) ([]string, error) {
	// Exposing the stolen Pod ports via a Service
	service, err := CreateService(K8SCli, *createdPod, donorUUID, stealerUUID)
	if err != nil {
		slog.Error("Failed to create Service", "error", err)
		return nil, err
	}
	if service == nil {
		slog.Info("No ports are exposed in the Pod", "pod", createdPod)
		return nil, err
	}
	slog.Info("Successfully created Service", "service", service)

	// Export the Service via Submariner
	serviceExport, err := ExportService(K8SConfig, service, donorUUID)
	if err != nil {
		slog.Error("Failed to export Service via submariner", "error", err)
		return nil, err
	}
	slog.Info("Successfully exported a service via submariner", "serviceExport", serviceExport)
	var fqdns []string
	for _, port := range service.Spec.Ports {
		fqdns = append(fqdns, fmt.Sprintf("%s.%s.svc.clusterset.local:%d", service.Name, service.Namespace, port.Port))
	}
	slog.Info("Access the service with the FQDN", "service", service, "fqdns", fqdns)
	return fqdns, nil // Return the FQDNs
}

func CreateService(cli *kubernetes.Clientset, pod corev1.Pod, donorUUID string, stealerUUID string) (*corev1.Service, error) {
	ports := common.PodExposedPorts(&pod)
	if len(ports) == 0 {
		slog.Info("No ports are exposed in the Pod", "pod", pod, "ports", ports)
		return nil, nil
	}

	sName := common.GenerateServiceNameForStolenPod(pod.Name)
	sNamespace := pod.Namespace

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sName,
			Namespace: sNamespace,
			Labels:    fetchStolenLabelsMap(&pod, donorUUID, stealerUUID),
		},
		Spec: corev1.ServiceSpec{
			Selector: pod.Labels,
			Ports:    ports,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}

	createdService, err := cli.CoreV1().Services(sNamespace).Create(context.TODO(), service, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create Service", "error", err)
		return nil, err
	}

	slog.Info("Successfully created Service", "service", createdService)
	return createdService, nil
}

func checkForAnyFailuresOrRestarts(cli *kubernetes.Clientset, pod *corev1.Pod, kv nats.KeyValue, pollKVKey string, timeoutInMin int) error {
	// Start polling the KV store for status updates
	defer func() {
		err := kv.Delete(pollKVKey)
		if err != nil && err != nats.ErrKeyNotFound {
			slog.Error("Failed to delete key from KV store", "error", err, "key", pollKVKey)
		}
	}()
	pollTimeout := time.After(time.Duration(timeoutInMin) * 60 * time.Second)
	for {
		select {
		case <-pollTimeout:
			slog.Error("Polling Timeout: No response sent!", "timeoutInMin", timeoutInMin)
			return fmt.Errorf("polling timeout: no response sent, timeoutInMin: %d", timeoutInMin)
		default:
			currentPod, err := cli.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			entry, getErr := kv.Get(pollKVKey)
			if apierrors.IsNotFound(err) {
				if getErr == nil && string(entry.Value()) == string(corev1.PodSucceeded) {
					slog.Warn("Pod not found. It executed successfully", "podName", pod.Name, "podNamespace", pod.Namespace)
					return nil
				} else {
					slog.Error("Pod not found", "podName", pod.Name, "podNamespace", pod.Namespace)
					return err
				}
			} else if err != nil {
				retries := 3
				for retries > 0 {
					time.Sleep(2 * time.Second)
					currentPod, err = cli.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
					if err == nil {
						break
					}
					retries--
				}
				if err != nil {
					slog.Error("Error retrieving Pod", "podName", pod.Name, "podNamespace", pod.Namespace, "error", err)
					return err
				}
			}

			podPollDetails := common.PodPollDetails{
				Status:    string(currentPod.Status.Phase),
				Duration:  metav1.Now().Sub(currentPod.Status.StartTime.Time).String(),
				Name:      currentPod.Name,
				Namespace: currentPod.Namespace,
			}
			slog.Info("polling the Pod", "podPollDetails", podPollDetails)
			podPollDetailsBytes, err := json.Marshal(podPollDetails)
			if err != nil {
				slog.Error("Failed to marshal podPollDetails", "error", err)
				return err
			}

			_, err = kv.Put(pollKVKey, podPollDetailsBytes)
			if err != nil {
				slog.Error("Failed to put KV entry", "error", err, "kvKey", pollKVKey)
				return nil
			}

			switch currentPod.Status.Phase {
			case corev1.PodFailed, corev1.PodUnknown:
				slog.Error("Pod has failed", "podName", pod.Name, "podNamespace", pod.Namespace)
				return nil
			case corev1.PodSucceeded:
				slog.Info("Pod has succeeded", "podName", pod.Name, "podNamespace", pod.Namespace)
				return nil
			}
			time.Sleep(5 * time.Second) // Poll every 5 seconds
		}
	}
}
