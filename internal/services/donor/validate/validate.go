package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/donor"
	"hirocrossclusterworkloads/pkg/metrics"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/mattbaird/jsonpatch"
	"github.com/nats-io/nats.go"
	admission "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
)

var (
	nodeSelectorUUID         = uuid.New().String()
	mutatePodNodeSelectorMap = map[string]string{
		"node-stolen": "true",
		"node-id":     nodeSelectorUUID,
	}
	mutatePodLablesMap = map[string]string{
		"is-pod-stolen": "true",
	}
	pollWaitTimeInMin = 10 // This has to be decide from some intelligence (like how long it takes to process the pod)
)

type validator struct {
	vconfig donor.DVConfig
	cli     *kubernetes.Clientset
}

func New(config donor.DVConfig) (donor.Validator, error) {
	clientset, err := common.GetK8sClientSet()
	if err != nil {
		return nil, err
	}
	return &validator{
		cli:     clientset,
		vconfig: config,
	}, nil
}

func (v *validator) Validate(ar admission.AdmissionReview) *admission.AdmissionResponse {
	slog.Info("No Validation for now")
	return &admission.AdmissionResponse{Allowed: true}
}

func (v *validator) Mutate(ar admission.AdmissionReview) *admission.AdmissionResponse {
	slog.Info("Make pod Invalid")
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		slog.Error("expect resource does not match", "expected", ar.Request.Resource, "received", podResource)
		return nil
	}
	if ar.Request.Operation != admission.Create {
		slog.Error("expect operation does not match", "expected", admission.Create, "received", ar.Request.Operation)
		return nil
	}
	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	if _, _, err := common.Deserializer.Decode(raw, nil, &pod); err != nil {
		slog.Error("failed to decode pod", "error", err)
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	// Check if the pod has already been processed
	_, exists := pod.Labels[common.StolenPodFailedLable]
	if exists {
		slog.Info("Pod has already been processed", "name", pod.Name, "namespace", pod.Namespace)
		return &admission.AdmissionResponse{Allowed: true}
	}
	isNotEligibleToSteal := common.IsLableExists(&pod, v.vconfig.LableToFilter)
	if isNotEligibleToSteal {
		slog.Info("Pod is not eligible to steal as it has the label", "name", pod.Name, "label", v.vconfig.LableToFilter)
		return &admission.AdmissionResponse{Allowed: true}
	}
	ignoreNamespaces := v.vconfig.IgnoreNamespaces
	slog.Info("Pod create event", "namespace", pod.Namespace, "name", pod.Name)
	uniqueIgnoredNamespaces := common.MergeUnique(ignoreNamespaces, common.K8SNamespaces)
	if common.IsItIgnoredNamespace(uniqueIgnoredNamespaces, pod.Namespace) {
		slog.Info("Ignoring as Pod belongs to Ignored Namespaces", "namespaces", ignoreNamespaces)
		return nil
	}

	natsConnect, js, kv, err := v.vconfig.Nconfig.GetNATSConnectJetStreamAndKeyValue()
	if err != nil {
		slog.Error("Failed to connect to NATS/JS/KV", "error", err, "natsConnect", natsConnect, "js", js, "kv", kv)
		return &admission.AdmissionResponse{Allowed: true}
	}
	//defer natsConnect.Close()
	//go func() {}()
	// Make the pod to be stolen
	kvKey := common.GenerateStealWorkloadKVKey(v.vconfig.DonorUUID, pod.Namespace, pod.Name)
	stealerUUID, err := Inform(&pod, v.vconfig, js, kv, kvKey)
	if err != nil && stealerUUID == "" {
		slog.Warn("Failed to make the pod to be stolen", "name", pod.Name, "namespace", pod.Namespace, "error", err)
		return &admission.AdmissionResponse{Allowed: true}
	}
	slog.Info("Pod got stolen", "name", pod.Name, "namespace", pod.Namespace, "stealerUUID", stealerUUID)

	go func() {
		pollKVKey := common.GeneratePollStealWorkloadKVKey(v.vconfig.DonorUUID, stealerUUID, pod.Namespace, pod.Name)
		err := pollStolenPodStatus(kv, pollKVKey, pollWaitTimeInMin)
		if err != nil {
			slog.Error("Failed to poll stolen pod status", "error", err)

			// Create the pod again by adding a label
			pod.Labels[common.StolenPodFailedLable] = "true"
			slog.Info("Creating the pod here itself", "name", pod.Name, "namespace", pod.Namespace)
			_, createErr := v.cli.CoreV1().Pods(pod.Namespace).Create(context.TODO(), &pod, metav1.CreateOptions{})
			if createErr != nil {
				slog.Error("Failed to create pod", "error", createErr)
			} else {
				slog.Info("Successfully created pod", "name", pod.Name, "namespace", pod.Namespace)
			}
		}
	}()

	//Mutate pod so that it won't be scheduled
	return mutatePod(&pod, v.vconfig.DonorUUID, stealerUUID)
}

func Inform(pod *corev1.Pod, vconfig donor.DVConfig, js nats.JetStreamContext,
	kv nats.KeyValue, kvKey string) (string, error) {
	// Store "Pending" in KV Store
	err := vconfig.Nconfig.PutKeyValue(kv, kvKey, common.DonorKVValuePending)
	if err != nil {
		slog.Error("Failed to put value in KV bucket: ", "error", err,
			"key", kvKey, "value", common.DonorKVValuePending)
		return "", err
	}

	donorPod := common.DonorPod{
		KVKey:     kvKey,
		DonorUUID: vconfig.DonorUUID,
		Pod:       pod,
		WaitTime:  10,
	}
	slog.Info("Created donorPod structure", "struct", donorPod)

	// Serialize the entire Pod metadata to JSON
	metadataJSON, err := json.Marshal(donorPod)
	if err != nil {
		slog.Error("Failed to serialize donorPod", "error", err, "donorPod", donorPod)
		return "", err
	}

	// Publish notification to JetStreams
	err = vconfig.Nconfig.PublishJSMessage(js, metadataJSON)
	if err != nil {
		slog.Error("Failed to publish pod metadata to JS", "error", err, "subject", vconfig.Nconfig.NATSSubject, "donorUUID", vconfig.DonorUUID)
		return "", err
	}
	slog.Info("Published Pod metadata to JS", "subject", vconfig.Nconfig.NATSSubject, "metadata", string(metadataJSON), "donorUUID", vconfig.DonorUUID)
	metrics.DonorPublishedTasksTotal.WithLabelValues(vconfig.DonorUUID).Inc()

	return WaitToGetPodStolen(vconfig.WaitToGetPodStolen, kv, kvKey, vconfig)
}

func WaitToGetPodStolen(waitTime int, kv nats.KeyValue, kvKey string, vconfig donor.DVConfig) (string, error) {
	waitTime = waitTime * 60 // Convert minutes to seconds
	slog.Info(fmt.Sprintf("Waiting for %d seconds for ACK", waitTime))
	for i := 0; i < waitTime; i++ {
		time.Sleep(1 * time.Second) // Wait for 1 second
		stealerUUID, err := vconfig.Nconfig.GetKeyValue(kv, kvKey)
		if err == nil && string(stealerUUID) != common.DonorKVValuePending {
			slog.Info("Published Pod metadata was processed", "donorUUID", vconfig.DonorUUID, "stealerUUID", stealerUUID)
			return stealerUUID, nil
		}
	}
	slog.Error("No Stealer is ready to stole within the wait time", "donorUUID", vconfig.DonorUUID, "WaitTimeForACK", waitTime)
	return "", fmt.Errorf("no stealer is ready to steale within the wait time %d for donorUUID: %s", waitTime, vconfig.DonorUUID)
}

func mutatePod(pod *corev1.Pod, donorUUID string, stealerUUID string) *admission.AdmissionResponse {
	originalPod := pod.DeepCopy()
	modifiedPod := originalPod.DeepCopy()

	// // Replace all containers with our dummy container
	// for i := range modifiedPod.Spec.Containers {
	// 	container := &modifiedPod.Spec.Containers[i]
	// 	container.Image = "busybox"
	// 	container.Command = []string{
	// 		"/bin/sh",
	// 		"-c",
	// 		"echo 'Pod got stolen' && sleep infinity",
	// 	}
	// }

	modifiedPod.Spec.NodeSelector = mutatePodNodeSelectorMap
	if modifiedPod.Labels == nil {
		modifiedPod.Labels = make(map[string]string)
	}
	for key, value := range mutatePodLablesMap {
		modifiedPod.Labels[key] = value
	}
	modifiedPod.Labels["donorUUID"] = donorUUID
	modifiedPod.Labels["stealerUUID"] = stealerUUID

	// Marshal the modified pod to JSON
	originalJSON, err := json.Marshal(originalPod)
	if err != nil {
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: fmt.Sprintf("Failed to marshal original pod: %v", err),
				Code:    http.StatusInternalServerError,
			},
		}
	}

	// Marshal the modified pod to JSON
	modifiedJSON, err := json.Marshal(modifiedPod)
	if err != nil {
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: fmt.Sprintf("Failed to marshal modified pod: %v", err),
				Code:    http.StatusInternalServerError,
			},
		}
	}

	// Create JSON Patch
	patch, err := jsonpatch.CreatePatch(originalJSON, modifiedJSON)
	if err != nil {
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: fmt.Sprintf("Failed to create patch: %v", err),
				Code:    http.StatusInternalServerError,
			},
		}
	}

	// Marshal the patch to JSON
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: fmt.Sprintf("Failed to marshal patch: %v", err),
				Code:    http.StatusInternalServerError,
			},
		}
	}

	// Return the AdmissionResponse with the mutated Pod
	return &admission.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admission.PatchType {
			pt := admission.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func pollStolenPodStatus(kv nats.KeyValue, pollKVKey string, timeoutInMin int) error {
	// Start polling the KV store for status updates
	pollTimeout := time.After(time.Duration(timeoutInMin) * 60 * time.Second)
	var pollDetails *common.PodPollDetails
	watcher, err := kv.Watch(pollKVKey)
	if err != nil {
		slog.Error("Failed to start watcher", "error", err)
		return err
	}
	for {
		select {
		case <-pollTimeout:
			slog.Error("Polling Timeout: No response received!")
			return fmt.Errorf("polling timeout: no response received, timeout: %d", timeoutInMin)
		case update := <-watcher.Updates():
			if update == nil {
				continue
			}
			value := update.Value()
			err := json.Unmarshal(value, &pollDetails)
			if err != nil {
				slog.Error("Failed to Unmarshal pollDetails", "error", err, "value", string(value))
				return err
			}
			slog.Info("Pod Poll details", "pollDetails", pollDetails)

			if pollDetails.Status == common.PodFinishedStatus {
				fmt.Println("Pod Processing Completed!")
				return nil
			} else if pollDetails.Status == common.PodFailedStatus {
				fmt.Println("Pod Processing Failed!")
				return fmt.Errorf("pod processing failed")
			}
		}
	}
}
