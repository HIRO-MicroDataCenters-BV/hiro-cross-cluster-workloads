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
	"k8s.io/apimachinery/pkg/runtime"
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
		StealResource(c.K8SCli, &pod, donorUUID, stealerUUID)
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

	createdResource, err := StealResource(c.K8SCli, &pod, donorUUID, stealerUUID)
	if err != nil {
		slog.Error("Failed to steal Resource", "error", err)
		msg.Nak()
	}
	slog.Info("Successfully stole the Resource", "resource", createdResource)

	metrics.StealerClaimedTasksTotal.WithLabelValues(stealerUUID).Inc()
	metrics.StolenTasksTotal.WithLabelValues(donorUUID, stealerUUID).Inc()

	// Watch the created pod for every 5 sec and send it status to donor
	// go func() {
	// 	pollKVKey := common.GeneratePollStealWorkloadKVKey(donorUUID, stealerUUID, pod.Namespace, pod.Name)
	// 	checkForAnyFailuresOrRestarts(c.K8SCli, createdPod, kv, pollKVKey, waitTime)
	// }()

	fqdn, ports, err := ExposeServiceAndExportViaSubmariner(c.K8SCli, c.K8SConfig, &createdResource, donorUUID, stealerUUID)
	if err != nil {
		slog.Error("Failed to expose Service and export via submariner", "error", err)
		return
	}
	slog.Info("Successfully exposed a Service and exported via submariner", "fqdn", fqdn, "ports", ports)

	servingStealerDetails.ExposedFQDN = fqdn
	servingStealerDetails.ExposedPorts = ports
	servingStealerDetailsBytes, err = json.Marshal(servingStealerDetails)
	if err != nil {
		slog.Error("Failed to marshal servingStealerDetails", "error", err)
		return
	}
	// Mark with servingStealerDetails in KV by this stealer
	for retries := 3; retries > 0; retries-- {
		_, err = kv.Update(kvKey, servingStealerDetailsBytes, entry.Revision())
		if err == nil {
			slog.Info("Successfully updated the KV bucket with exposed FQDNs", "fqdn", fqdn, "ports", ports)
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
		slog.Error("Failed to update value in KV bucket with FQDN: ", "error", err)
		return
	}
	slog.Info("Successfully updated the KV bucket with exposed FQDN", "fqdn", fqdn, "ports", ports)
}

func StealResource(cli *kubernetes.Clientset, resource runtime.Object, donorUUID string, stealerUUID string) (runtime.Object, error) {
	var namespace, name string
	var labels map[string]string

	switch r := resource.(type) {
	case *corev1.Pod:
		namespace = r.Namespace
		name = r.Name
		labels = r.Labels
	case *appsv1.Deployment:
		namespace = r.Namespace
		name = r.Name
		labels = r.Labels
	default:
		return nil, fmt.Errorf("StealResource: unsupported resource type")
	}

	success, err := CreateNamespace(cli, namespace)
	if !success || err != nil {
		slog.Error("Error occurred", "error", err)
		return nil, err
	}

	sterilizeResourceInplace(&resource, donorUUID, stealerUUID)

	// Check if the resource already exists
	switch r := resource.(type) {
	case *corev1.Pod:
		existingPod, err := cli.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err == nil {
			if common.AreMapsEqual(existingPod.Labels, labels) {
				slog.Info("Pod already stolen", "podName", name, "podNamespace", namespace)
				return existingPod, nil
			} else {
				slog.Error("Cannot Steal; Same Pod exists with the specified details", "podName", name, "podNamespace", namespace)
				return nil, fmt.Errorf("cannot steal; same pod already exists with the specified details: podName=%s, podNamespace=%s", name, namespace)
			}
		} else if !apierrors.IsNotFound(err) {
			slog.Error("Failed to get Pod with specified details", "podName", name, "podNamespace", namespace, "error", err)
			return nil, err
		}

		// Create the Pod in Kubernetes
		createdPod, err := cli.CoreV1().Pods(namespace).Create(context.TODO(), r, metav1.CreateOptions{})
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

	case *appsv1.Deployment:
		existingDeployment, err := cli.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err == nil {
			if common.AreMapsEqual(existingDeployment.Labels, labels) {
				slog.Info("Deployment already stolen", "deploymentName", name, "deploymentNamespace", namespace)
				return existingDeployment, nil
			} else {
				slog.Error("Cannot Steal; Same Deployment exists with the specified details", "deploymentName", name, "deploymentNamespace", namespace)
				return nil, fmt.Errorf("cannot steal; same deployment already exists with the specified details: deploymentName=%s, deploymentNamespace=%s", name, namespace)
			}
		} else if !apierrors.IsNotFound(err) {
			slog.Error("Failed to get Deployment with specified details", "deploymentName", name, "deploymentNamespace", namespace, "error", err)
			return nil, err
		}

		// Create the Deployment in Kubernetes
		createdDeployment, err := cli.AppsV1().Deployments(namespace).Create(context.TODO(), r, metav1.CreateOptions{})
		if err != nil {
			slog.Error("Failed to create Deployment", "error", err)
			return nil, err
		}

		if !isResourceSuccessfullyRunning(cli, createdDeployment.Namespace, createdDeployment.Name, "Deployment", 5) {
			slog.Error("Deployment is not running within 5 min", "deployment", createdDeployment)
			return nil, fmt.Errorf("deployment is not running within 5 min: deploymentName=%s, deploymentNamespace=%s", createdDeployment.Name, createdDeployment.Namespace)
		}
		slog.Info("Successfully stole the workload", "deployment", createdDeployment)
		return createdDeployment, nil
	}

	return nil, fmt.Errorf("StealResource: unsupported resource type")
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

func sterilizeResourceInplace(resource *runtime.Object, donorUUID string, stealerUUID string) {
	switch resource := (*resource).(type) {
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
		Labels:      fetchStolenLabelsMap(pod.Labels, donorUUID, stealerUUID),
		Annotations: pod.Annotations,
	}
	pod.ObjectMeta = newPodObjectMeta
}

func sterilizeDeploymentInplace(deployment *appsv1.Deployment, donorUUID string, stealerUUID string) {
	newDeploymentObjectMeta := metav1.ObjectMeta{
		Name:        deployment.Name,
		Namespace:   deployment.Namespace,
		Labels:      fetchStolenLabelsMap(deployment.Labels, donorUUID, stealerUUID),
		Annotations: deployment.Annotations,
	}
	deployment.ObjectMeta = newDeploymentObjectMeta
}

func fetchStolenLabelsMap(lables map[string]string, donorUUID string, stealerUUID string) map[string]string {
	common.StolenPodLablesMap["donorUUID"] = donorUUID
	common.StolenPodLablesMap["stealerUUID"] = stealerUUID
	return common.MergeMaps(lables, common.StolenPodLablesMap)
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
	resource *runtime.Object, donorUUID string, stealerUUID string) (string, []int, error) {
	// Exposing the stolen Pod ports via a Service
	service, err := CreateService(K8SCli, *resource, donorUUID, stealerUUID)
	if err != nil {
		slog.Error("Failed to create Service", "error", err)
		return "", nil, err
	}
	if service == nil {
		slog.Info("No ports are exposed in the resource", "resource", resource)
		return "", nil, err
	}
	slog.Info("Successfully created Service", "service", service)

	// Export the Service via Submariner
	serviceExport, err := ExportService(K8SConfig, service, donorUUID)
	if err != nil {
		slog.Error("Failed to export Service via submariner", "error", err)
		return "", nil, err
	}
	slog.Info("Successfully exported a service via submariner", "serviceExport", serviceExport)
	fqdn := common.ResourceExposedFQDN(service)
	var ports []int
	for _, port := range service.Spec.Ports {
		ports = append(ports, int(port.Port))
	}
	slog.Info("Access the service with the FQDN", "service", service, "fqdn", fqdn, "ports", ports)
	return fqdn, ports, nil
}

func CreateService(cli *kubernetes.Clientset, resource runtime.Object, donorUUID string, stealerUUID string) (*corev1.Service, error) {
	ports := common.ResourceExposedPorts(resource)
	if len(ports) == 0 {
		slog.Info("No ports are exposed in the Resource", "resource", resource, "ports", ports)
		return nil, nil
	}

	switch resource := resource.(type) {
	case *corev1.Pod, *appsv1.Deployment:
		var sName, sNamespace string
		var rLabels, sLabels map[string]string
		switch r := resource.(type) {
		case *corev1.Pod:
			sName = common.GenerateServiceNameForStolenPod(r.Name)
			sNamespace = r.Namespace
			rLabels = r.Labels
			sLabels = fetchStolenLabelsMap(r.Labels, donorUUID, stealerUUID)
		case *appsv1.Deployment:
			sName = common.GenerateServiceNameForStolenPod(r.Name)
			sNamespace = r.Namespace
			rLabels = r.Labels
			sLabels = fetchStolenLabelsMap(r.Labels, donorUUID, stealerUUID)
		default:
			slog.Error("CreateService: Unsupported resource type")
			return nil, fmt.Errorf("CreateService: unsupported resource type")
		}

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sName,
				Namespace: sNamespace,
				Labels:    sLabels,
			},
			Spec: corev1.ServiceSpec{
				Selector: rLabels,
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
	return nil, fmt.Errorf("CreateService: unsupported resource type")
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
