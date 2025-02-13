package validate

import (
	"encoding/json"
	"fmt"
	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/donor"
	"log/slog"
	"net/http"
	"strings"
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
	isNotEligibleToSteal := isLableExists(&pod, v.vconfig.LableToFilter)
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

	//go func() {}()
	// Make the pod to be stolen
	stolerUUID, err := Inform(&pod, v.vconfig)
	if err != nil && stolerUUID == "" {
		slog.Warn("Failed to make the pod to be stolen", "name", pod.Name, "namespace", pod.Namespace, "error", err)
		return &admission.AdmissionResponse{Allowed: true}
	}
	slog.Info("Pod got stolen", "name", pod.Name, "namespace", pod.Namespace, "stealerUUID", stolerUUID)

	//Mutate pod so that it won't be scheduled
	return mutatePod(&pod, v.vconfig.DonorUUID, stolerUUID)
}

func isLableExists(pod *corev1.Pod, lable string) bool {
	value, ok := pod.Labels[lable]
	if !ok || strings.ToLower(value) == "false" {
		return false
	}
	return true
}

func Inform(pod *corev1.Pod, vconfig donor.DVConfig) (string, error) {
	// Connect to NATS server
	natsConnect, err := nats.Connect(vconfig.Nconfig.NATSURL)
	if err != nil {
		slog.Error("Failed to connect to NATS server: ", "error", err)
		return "", err
	}
	defer natsConnect.Close()
	slog.Info("Connected to NATS server")

	// Connect to JetStreams
	js, err := vconfig.Nconfig.FetchJetStream(natsConnect)
	if err != nil {
		slog.Error("Failed to connect to JetStreams server: ", "error", err)
		return "", err
	}

	jsStreamName := "Stream" + vconfig.Nconfig.NATSSubject
	// Create a stream for message processing
	vconfig.Nconfig.CreateJetStreamStream(js, jsStreamName)

	// Create or get the KV Store for message tracking
	kv, err := vconfig.Nconfig.CreateOrGetKeyValueStore(js, common.JetStreamBucket)
	if err != nil {
		slog.Error("Failed to get KeyValue: ", "error", err)
		return "", err
	}

	// Store "Pending" in KV Store
	err = vconfig.Nconfig.PutKeyValue(kv, vconfig.DonorUUID, "Pending")
	if err != nil {
		slog.Error("Failed to put value in KV bucket: ", "error", err)
	}

	donorPod := donor.DonorPod{
		DonorUUID: vconfig.DonorUUID,
		Pod:       pod,
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
		slog.Error("Failed to publish message to JS", "error", err, "subject", vconfig.Nconfig.NATSSubject, "donorUUID", vconfig.DonorUUID)
		return "", err
	}

	slog.Info("Published Pod metadata to JS", "subject", vconfig.Nconfig.NATSSubject, "metadata", string(metadataJSON), "donorUUID", vconfig.DonorUUID)

	return WaitToGetPodStolen(vconfig.WaitToGetPodStolen, kv, vconfig)
}

func WaitToGetPodStolen(waitTime int, kv nats.KeyValue, vconfig donor.DVConfig) (string, error) {
	slog.Info(fmt.Sprintf("Waiting for %d seconds for ACK", waitTime))
	for i := 0; i < waitTime; i++ {
		time.Sleep(1 * time.Second) // Wait for 1 second
		stealerUUID, err := vconfig.Nconfig.GetKeyValue(kv, vconfig.DonorUUID)
		if err == nil && string(stealerUUID) != "Pending" {
			slog.Info("Published Pod metadata was processed", "donorUUID", vconfig.DonorUUID, "stealerUUID", stealerUUID)
			return stealerUUID, nil
		}
	}
	slog.Error("No Stealer is ready to stole within the wait time", "donorUUID", vconfig.DonorUUID, "WaitTimeForACK", waitTime)
	return "", fmt.Errorf("no Stealer is ready to stole within the wait time for donorUUID: %s", vconfig.DonorUUID)
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
