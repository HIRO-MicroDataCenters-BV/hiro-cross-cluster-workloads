package informer

import (
	"context"
	"encoding/json"
	"log/slog"

	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/stealer"
	"hirocrossclusterworkloads/pkg/metrics"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	watch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type notify struct {
	config    stealer.SIConfig
	k8scli    *kubernetes.Clientset
	k8sconfig *rest.Config
}

func New(config stealer.SIConfig) (*notify, error) {
	clientset, k8sconf, err := common.GetK8sClientAndConfigSet()
	if err != nil {
		return nil, err
	}
	return &notify{
		k8scli:    clientset,
		k8sconfig: k8sconf,
		config:    config,
	}, nil
}

func (n *notify) Start(stopChan chan<- bool) error {
	defer func() { stopChan <- true }()

	ignoreNamespaces := n.config.IgnoreNamespaces
	nsubject := n.config.Nclient.NATSSubject
	// Connect to NATS server
	natsConnect, err := n.config.Nclient.FetchNATSConnect()
	if err != nil {
		slog.Error("Failed to connect to NATS server: ", "error", err)
		return err
	}
	defer natsConnect.Close()
	slog.Info("Connected to NATS server")

	slog.Info("Watching for Pod events")
	podWatch, err := n.k8scli.CoreV1().Pods("").Watch(context.Background(), metav1.ListOptions{})
	if err != nil {
		slog.Error("Failed to watch pods: ", "error", err)
		return err
	}
	defer podWatch.Stop()

	slog.Info("Listening for Pod deletion events...")
	for event := range podWatch.ResultChan() {
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}

		if event.Type == watch.Deleted {
			slog.Info("Received", "event", event)
			uniqueIgnoredNamespaces := common.MergeUnique(ignoreNamespaces, common.K8SNamespaces)
			if common.IsItIgnoredNamespace(uniqueIgnoredNamespaces, pod.Namespace) {
				slog.Info("Ignoring as Pod belongs to Ignored Namespaces", "namespaces", ignoreNamespaces)
				continue
			}

			for label := range common.StolenPodLablesMap {
				_, ok := pod.Labels[label]
				if !ok {
					slog.Info("Ignoring as Pod is not stolen one", "namespace", pod.Namespace, "name", pod.Name)
					continue
				}
			}

			slog.Info("Pod is stolen one, publishing results to NATS", "subject", nsubject,
				"namespace", pod.Namespace, "name", pod.Name, "labels", pod.Labels)

			podResults := n.generatePodResults(pod)
			completionTime := podResults.Results.CompletionTime.Time.Second()
			metrics.StealerProcessingDuration.WithLabelValues(n.config.StealerUUID).Observe(float64(completionTime))
			// Serialize the entire PodResults to JSON
			metadataJSON, err := json.Marshal(podResults)
			if err != nil {
				slog.Error("Failed to serialize PodResults", "error", err, "podResults", podResults)
				continue
			}

			// Publish notification to NATS
			err = natsConnect.Publish(nsubject, metadataJSON)
			if err != nil {
				slog.Error("Failed to publish message to NATS", "error", err, "subject", nsubject, "podResults", podResults)
				continue
			}
			slog.Info("Publishing the finished Pod Results to NATS", "subject", nsubject, "podresults", string(metadataJSON))
		}
	}
	return nil
}

func (n *notify) generatePodResults(pod *corev1.Pod) common.PodResults {
	return common.PodResults{
		Results: common.Result{
			StealerUUID:    n.config.StealerUUID,
			DonorUUID:      pod.Labels["donorUUID"],
			Message:        "Pod Execution Completed",
			Status:         "Success",
			StartTime:      *pod.Status.StartTime,
			CompletionTime: metav1.Now(),
			Duration:       metav1.Now().Sub(pod.Status.StartTime.Time).String(),
		},
		Pod: pod,
	}
}
