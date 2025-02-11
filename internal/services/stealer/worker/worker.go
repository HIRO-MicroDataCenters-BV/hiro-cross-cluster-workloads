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

	nats "github.com/nats-io/nats.go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Consume struct {
	Config stealer.SWConfig
	Cli    *kubernetes.Clientset
}

func New(config stealer.SWConfig) (stealer.Stealer, error) {
	clientset, err := common.GetK8sClientSet()
	if err != nil {
		return nil, err
	}
	//Consume implements Stealer interface
	return &Consume{
		Cli:    clientset,
		Config: config,
	}, nil
}

func (c *Consume) Start(stopChan chan<- bool) error {
	defer func() { stopChan <- true }()

	// Connect to NATS server
	natsConnect, err := c.Config.Nclient.FetchNATSConnect()
	if err != nil {
		slog.Error("Failed to connect to NATS server: ", "error", err)
		return err
	}
	defer natsConnect.Close()

	// Connect to JetStreams
	js, err := c.Config.Nclient.FetchJetStream(natsConnect)
	if err != nil {
		slog.Error("Failed to connect to JetStreams server: ", "error", err)
		return err
	}

	jsStreamName := "Stream" + c.Config.Nclient.NATSSubject
	// Create a stream for message processing
	c.Config.Nclient.CreateJetStreamStream(js, jsStreamName)

	// Create or get the KV Store for message tracking
	kv, err := c.Config.Nclient.CreateOrGetKeyValueStore(js, common.JetStreamBucket)
	if err != nil {
		slog.Error("Failed to get KeyValue: ", "error", err)
		return err
	}

	slog.Info("Subscribe to Pod stealing messages...", "stream", jsStreamName,
		"queue", common.JetStreamQueue, "subject", c.Config.Nclient.NATSSubject)

	// Queue Group ensures only one consumer gets a message
	js.QueueSubscribe(c.Config.Nclient.NATSSubject, common.JetStreamQueue, func(msg *nats.Msg) {
		var pod corev1.Pod
		var donorPod common.DonorPod
		var donorUUID string
		var stealerUUID = c.Config.StealerUUID
		slog.Info("Received message", "subject", c.Config.Nclient.NATSSubject, "data", string(msg.Data))

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
		_, err = c.Cli.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
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
		donorUUID = donorPod.DonorUUID
		slog.Info("Deserialized donorUUID", "donorUUID", donorUUID)

		// Check if message is already processed
		entry, err := kv.Get(donorUUID)
		if err == nil && string(entry.Value()) != "Pending" {
			otherStealerUUID := string(entry.Value())
			if otherStealerUUID == stealerUUID {
				slog.Info("Skipping Pod, I am already processed it", "podName", pod.Name,
					"podNamespace", pod.Namespace, "stealerUUID", stealerUUID)
				StealPod(c.Cli, pod, donorUUID, stealerUUID)
				msg.Ack()
			} else {
				slog.Info("Skipping Pod, already processed by another stealer", "podName", pod.Name,
					"podNamespace", pod.Namespace, "otherStealerUUID", otherStealerUUID)
			}
			return
		}

		// Mark with stealerUUID in KV by this stealer
		_, err = kv.Put(donorUUID, []byte(stealerUUID))
		if err != nil && !errors.Is(err, nats.ErrKeyExists) {
			slog.Error("Failed to put value in KV bucket: ", "error", err)
			msg.Nak()
		}

		_, err = StealPod(c.Cli, pod, donorUUID, stealerUUID)
		if err != nil {
			slog.Error("Failed to steal Pod", "error", err)
			msg.Nak()
		}

		// Acknowledge JetStream message
		msg.Ack()

	})
	select {}
}

func StealPod(cli *kubernetes.Clientset, pod corev1.Pod, donorUUID string, stealerUUID string) (*corev1.Pod, error) {
	success, err := CreateNamespace(cli, pod.Namespace)
	if !success || err != nil {
		slog.Error("Error occurred", "error", err)
		return nil, err
	}

	sterilizePodInplace(&pod, donorUUID, stealerUUID)

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

	if !isPodSuccesfullyRunning(cli, pod.Namespace, pod.Name) {
		slog.Error("Pod is not running within 5 min", "pod", createdPod)
		return nil, fmt.Errorf("pod is not running within 5 min: podName=%s, podNamespace=%s", pod.Name, pod.Namespace)
	}
	slog.Info("Successfully stole the workload", "pod", createdPod)
	return createdPod, nil
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

func sterilizePodInplace(pod *corev1.Pod, donorUUID string, stealerUUID string) {
	newPodObjectMeta := metav1.ObjectMeta{
		Name:        pod.Name,
		Namespace:   pod.Namespace,
		Labels:      fetchStolenPodLablesMap(*pod, donorUUID, stealerUUID),
		Annotations: pod.Annotations,
	}
	pod.ObjectMeta = newPodObjectMeta
}

func fetchStolenPodLablesMap(pod corev1.Pod, donorUUID string, stealerUUID string) map[string]string {
	common.StolenPodLablesMap["donorUUID"] = donorUUID
	common.StolenPodLablesMap["stealerUUID"] = stealerUUID
	return common.MergeMaps(pod.Labels, common.StolenPodLablesMap)
}

// pollPodStatus polls the status of a Pod until it is Running or a timeout occurs
func isPodSuccesfullyRunning(clientset *kubernetes.Clientset, namespace, name string) bool {
	timeout := time.After(5 * time.Minute)    // Timeout after 5 minutes
	ticker := time.NewTicker(5 * time.Second) // Poll every 5 seconds
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			slog.Error("Timeout waiting for Pod to reach Running state", "namespace", namespace, "name", name)
			return false
		case <-ticker.C:
			pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				slog.Error("Failed to get Pod status", "namespace", namespace, "name", name, "error", err)
				return false
			}

			slog.Info("Pod status", "namespace", namespace, "name", name, "phase", pod.Status.Phase)

			// Check if the Pod is Running
			if pod.Status.Phase == corev1.PodRunning {
				slog.Info("Pod is now Running", "namespace", namespace, "name", name)
				return true
			}
		}
	}
}
