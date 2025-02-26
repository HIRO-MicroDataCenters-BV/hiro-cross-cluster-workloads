package worker

import (
	"context"
	"encoding/json"
	"log/slog"

	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/donor"
	"hirocrossclusterworkloads/pkg/metrics"

	nats "github.com/nats-io/nats.go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Consume struct {
	Config    donor.DWConfig
	K8SCli    *kubernetes.Clientset
	K8SConfig *rest.Config
}

func New(config donor.DWConfig) (*Consume, error) {
	clientset, k8sconfig, err := common.GetK8sClientAndConfigSet()
	if err != nil {
		return nil, err
	}
	return &Consume{
		K8SCli:    clientset,
		K8SConfig: k8sconfig,
		Config:    config,
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

	slog.Info("Subscribe to stolen Pod results messages...", "subject", c.Config.Nclient.NATSSubject)
	// // Subscribe to the subject
	natsConnect.Subscribe(c.Config.Nclient.NATSSubject, func(msg *nats.Msg) {
		var pod corev1.Pod
		var podResults common.PodResults
		slog.Info("Received message", "subject", c.Config.Nclient.NATSSubject, "data", string(msg.Data))

		// Deserialize the entire pod metadata to JSON
		err := json.Unmarshal(msg.Data, &podResults)
		if err != nil {
			slog.Error("Failed to Unmarshal podResults from rawData", "error", err, "rawData", string(msg.Data))
			return
		}
		pod = *podResults.Pod
		slog.Info("Deserialized Pod", "pod", pod)
		if pod.Labels["donorUUID"] != c.Config.DonorUUID {
			slog.Error("Pod is not intended for this donor", "podName", pod.Name, "podNamespace", pod.Namespace)
			return

		}
		results := podResults.Results
		slog.Info("Stolen Pod Execution Results", "results", results)

		// Check if the Pod exists or not
		currentPod, err := c.K8SCli.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			slog.Warn("Pod not found. It might have been deleted before acknowledging the message, possibly due to a worker restart",
				"podName", pod.Name, "podNamespace", pod.Namespace)
			msg.Ack()
			return
		} else if pod.Labels["stealerUUID"] != currentPod.Labels["stealerUUID"] ||
			pod.Labels["donorUUID"] != currentPod.Labels["donorUUID"] {
			slog.Error("Stolen Pod Lables are not matching with the current Pod", "podName", pod.Name,
				"podNamespace", pod.Namespace, "currentPodLables", currentPod.Labels, "stolenPodLables", pod.Labels)
			return
		} else if err != nil {
			slog.Error("Error retrieving Pod", "podName", pod.Name, "podNamespace", pod.Namespace, "error", err)
			return
		}

		if currentPod.Status.Phase != corev1.PodPending {
			slog.Error("Pod is not in Pending state", "podName", pod.Name, "podNamespace", pod.Namespace,
				"phase", currentPod.Status.Phase)
			return
		}

		metrics.DonorResultsReceivedTotal.WithLabelValues(c.Config.DonorUUID).Inc()
		// Delete the pending Pod here
		err = c.K8SCli.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		if err != nil {
			slog.Error("Failed to delete Pod", "podName", pod.Name, "podNamespace", pod.Namespace, "error", err)
			return
		}
		slog.Info("Deleted the pending Pod", "podName", pod.Name, "podNamespace", pod.Namespace)

		// Acknowledge message. This will remove the message from the NATS server
		msg.Ack()
	})
	select {}
}
