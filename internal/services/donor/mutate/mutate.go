package mutate

import (
	"context"
	"encoding/json"
	"fmt"
	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/donor"
	"hirocrossclusterworkloads/pkg/metrics"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mattbaird/jsonpatch"
	"github.com/nats-io/nats.go"
	admission "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"

	"maps"

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
	VConfig donor.DVConfig
	K8Scli  *kubernetes.Clientset
}

func New(config donor.DVConfig) (donor.Validator, error) {
	clientset, _, err := common.GetK8sClientAndConfigSet()
	if err != nil {
		return nil, err
	}
	return &validator{
		K8Scli:  clientset,
		VConfig: config,
	}, nil
}

func (v *validator) Validate(ar admission.AdmissionReview) *admission.AdmissionResponse {
	slog.Info("No Validation for now")
	return &admission.AdmissionResponse{Allowed: true}
}

func (v *validator) Mutate(ar admission.AdmissionReview) *admission.AdmissionResponse {
	slog.Info("Make pod Invalid")
	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	jobResource := metav1.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	expectedResources := []metav1.GroupVersionResource{podResource, jobResource}
	if ar.Request.Resource != podResource && ar.Request.Resource != jobResource {
		slog.Error("expected resource does not match", "expected", expectedResources, "received", ar.Request.Resource)
		return nil
	}
	if ar.Request.Operation != admission.Create {
		slog.Error("expect operation does not match", "expected", admission.Create, "received", ar.Request.Operation)
		return nil
	}
	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	job := batchv1.Job{}

	if ar.Request.Kind.Kind == "Pod" {
		_, _, err := common.Deserializer.Decode(raw, nil, &pod)
		if err != nil {
			slog.Error("failed to decode pod", "error", err)
			return &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		return v.mutateResourceWrapper(&pod)
	} else if ar.Request.Kind.Kind == "Job" {
		if _, _, err := common.Deserializer.Decode(raw, nil, &job); err != nil {
			slog.Error("failed to decode service", "error", err)
			return &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		return v.mutateResourceWrapper(&job)
	}
	return nil
}

func (v *validator) mutateResourceWrapper(resource runtime.Object) *admission.AdmissionResponse {
	var resourceName, resourceNamespace, resourceType string
	var labels map[string]string
	var isNotEligibleToSteal bool

	switch resourceObj := resource.(type) {
	case *corev1.Pod:
		resourceName = resourceObj.Name
		resourceNamespace = resourceObj.Namespace
		labels = resourceObj.Labels
		isNotEligibleToSteal = common.IsLabelExists(resourceObj, v.VConfig.LableToFilter) || common.IsLabelExists(resourceObj, common.StolenPodFailedLable)
		resourceType = "Pod"
	case *batchv1.Job:
		resourceName = resourceObj.Name
		resourceNamespace = resourceObj.Namespace
		labels = resourceObj.Labels
		isNotEligibleToSteal = common.IsLabelExists(resourceObj, v.VConfig.LableToFilter) || common.IsLabelExists(resourceObj, common.StolenPodFailedLable)
		resourceType = "Job"
	default:
		return &admission.AdmissionResponse{Allowed: true}
	}

	if isNotEligibleToSteal {
		slog.Info(fmt.Sprintf("%s is not eligible to steal as it has the label", resourceType), "name", resourceName, "label", v.VConfig.LableToFilter)
		return &admission.AdmissionResponse{Allowed: true}
	}

	ignoreNamespaces := v.VConfig.IgnoreNamespaces
	slog.Info(fmt.Sprintf("%s create event", resourceType), "namespace", resourceNamespace, "name", resourceName)
	uniqueIgnoredNamespaces := common.MergeUnique(ignoreNamespaces, common.K8SNamespaces)
	if common.IsItIgnoredNamespace(uniqueIgnoredNamespaces, resourceNamespace) {
		slog.Info(fmt.Sprintf("Ignoring as %s belongs to Ignored Namespaces", resourceType), "namespaces", ignoreNamespaces)
		return nil
	}

	slog.Info("Mutate the resource", "name", resourceName, "namespace", resourceNamespace, "resourceType", resourceType, "labels", labels)

	natsConnect, js, kv, err := v.VConfig.Nconfig.GetNATSConnectJetStreamAndKeyValue()
	if err != nil {
		slog.Error("Failed to connect to NATS/JS/KV", "error", err, "natsConnect", natsConnect, "js", js, "kv", kv)
		return &admission.AdmissionResponse{Allowed: true}
	}

	kvKey := common.GenerateStealWorkloadKVKey(v.VConfig.DonorUUID, resourceNamespace, resourceName)
	stealerUUID, err := InformAboutResource(resource, v.VConfig, js, kv, kvKey)
	if err != nil && stealerUUID == "" {
		slog.Warn(fmt.Sprintf("Failed to make the %s to be stolen", resourceType), "name", resourceName, "namespace", resourceNamespace, "error", err)
		return &admission.AdmissionResponse{Allowed: true}
	}
	slog.Info(fmt.Sprintf("%s got stolen", resourceType), "name", resourceName, "namespace", resourceNamespace, "stealerUUID", stealerUUID)

	go func() {
		// pollKVKey := common.GeneratePollStealWorkloadKVKey(v.VConfig.DonorUUID, stealerUUID, resourceNamespace, resourceName)
		// err := pollStolenPodStatus(kv, pollKVKey, pollWaitTimeInMin)
		// if err != nil {
		// 	slog.Error(fmt.Sprintf("Failed to poll stolen %s status", resourceType), "error", err)
		// 	createResourceWithLabel(resource, resourceType, resourceName, resourceNamespace, labels, v.K8Scli)
		// }
	}()

	return v.mutateResource(resource, kv, kvKey, stealerUUID)
}

func InformAboutResource(resource runtime.Object, vconfig donor.DVConfig, js nats.JetStreamContext,
	kv nats.KeyValue, kvKey string) (string, error) {
	// Store "Pending" in KV Store
	emptyStealerDetails := common.StealerDetails{
		StealerUUID:  common.DonorKVValuePending,
		KVKey:        kvKey,
		ExposedFQDNs: []string{},
	}
	emptyStealerDetailsBytes, err := json.Marshal(emptyStealerDetails)
	if err != nil {
		slog.Error("Failed to marshal emptyStealerDetails", "error", err)
		return "", err
	}

	_, err = kv.Put(kvKey, emptyStealerDetailsBytes)
	if err != nil {
		slog.Error("Failed to put value in KV bucket: ", "error", err,
			"key", kvKey, "value", emptyStealerDetails)
		return "", err
	}

	var metadataJSON []byte
	var donorUUID string

	switch resourceObj := resource.(type) {
	case *corev1.Pod:
		resourceObj.Labels["donorUUID"] = vconfig.DonorUUID
		resourceObj.Labels["serviceName"] = common.GenerateServiceNameForStolenPod(resourceObj.Name)
		donorPod := common.DonorPod{
			DonorDetails: common.DonorDetails{
				DonorUUID: vconfig.DonorUUID,
				KVKey:     kvKey,
				WaitTime:  10,
			},
			Pod: resourceObj,
		}
		metadataJSON, err = json.Marshal(donorPod)
		donorUUID = vconfig.DonorUUID
	case *batchv1.Job:
		resourceObj.Labels["donorUUID"] = vconfig.DonorUUID
		donorJob := common.DonorJob{
			DonorDetails: common.DonorDetails{
				DonorUUID: vconfig.DonorUUID,
				KVKey:     kvKey,
				WaitTime:  10,
			},
			Job: resourceObj,
		}
		metadataJSON, err = json.Marshal(donorJob)
		donorUUID = vconfig.DonorUUID
	default:
		return "", fmt.Errorf("unsupported resource type")
	}

	if err != nil {
		slog.Error("Failed to serialize resource", "error", err, "resource", resource)
		return "", err
	}

	// Publish notification to JetStreams
	err = vconfig.Nconfig.PublishJSMessage(js, metadataJSON)
	if err != nil {
		slog.Error("Failed to publish resource metadata to JS", "error", err, "subject", vconfig.Nconfig.NATSSubject, "donorUUID", donorUUID)
		return "", err
	}
	slog.Info("Published resource metadata to JS", "subject", vconfig.Nconfig.NATSSubject, "metadata", string(metadataJSON), "donorUUID", donorUUID)
	metrics.DonorPublishedTasksTotal.WithLabelValues(donorUUID).Inc()

	return WaitToGetResourceStolen(vconfig.WaitToGetPodStolen, kv, kvKey, vconfig)
}

func WaitToGetResourceStolen(waitTime int, kv nats.KeyValue, kvKey string, vconfig donor.DVConfig) (string, error) {
	waitTime = waitTime * 60 // Convert minutes to seconds
	slog.Info(fmt.Sprintf("Waiting for %d seconds for ACK", waitTime))
	for i := 0; i < waitTime; i++ {
		time.Sleep(1 * time.Second) // Wait for 1 second
		entry, err := kv.Get(kvKey)
		if err != nil {
			slog.Error("Failed to get value from KV store", "error", err, "key", kvKey)
			return "", err
		}
		servingStealerDetails := common.StealerDetails{}
		err = json.Unmarshal(entry.Value(), &servingStealerDetails)
		if err != nil {
			slog.Error("Failed to unmarshal servingStealerDetails", "error", err)
			return "", err
		}
		stealerUUID := servingStealerDetails.StealerUUID
		if stealerUUID != common.DonorKVValuePending {
			slog.Info("Published resource metadata was processed", "donorUUID", vconfig.DonorUUID, "stealerUUID", stealerUUID)
			return stealerUUID, nil
		}
	}
	slog.Error("No Stealer is ready to stole within the wait time", "donorUUID", vconfig.DonorUUID, "WaitTimeForACK", waitTime)
	return "", fmt.Errorf("no stealer is ready to steale within the wait time %d for donorUUID: %s", waitTime, vconfig.DonorUUID)
}

func (v *validator) mutateResource(resource runtime.Object, kv nats.KeyValue, kvKey string, stealerUUID string) *admission.AdmissionResponse {
	var originalJSON, modifiedJSON []byte
	var err error
	var donorUUID = v.VConfig.DonorUUID

	switch resourceObj := resource.(type) {
	case *corev1.Pod:
		originalPod := resourceObj.DeepCopy()
		modifiedPod := originalPod.DeepCopy()

		//  // Replace all containers with our dummy container
		// for i := range modifiedPod.Spec.Containers {
		// 	container := &modifiedPod.Spec.Containers[i]
		// 	container.Image = "busybox"
		// 	container.Command = []string{
		// 		"/bin/sh",
		// 		"-c",
		// 		"echo 'Pod got stolen' && sleep infinity",
		// 	}
		// }

		// Modify nodeSelector to make sure the pod in pending state
		modifiedPod.Spec.NodeSelector = mutatePodNodeSelectorMap
		if modifiedPod.Labels == nil {
			modifiedPod.Labels = make(map[string]string)
		}
		maps.Copy(modifiedPod.Labels, originalPod.Labels)
		maps.Copy(modifiedPod.Labels, mutatePodLablesMap)
		maps.Copy(modifiedPod.Labels, map[string]string{
			"donorUUID":   donorUUID,
			"stealerUUID": stealerUUID,
		})
		originalPod.Labels = map[string]string{}

		originalJSON, err = json.Marshal(originalPod)
		if err != nil {
			return &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: fmt.Sprintf("Failed to marshal original pod: %v", err),
					Code:    http.StatusInternalServerError,
				},
			}
		}

		modifiedJSON, err = json.Marshal(modifiedPod)
		if err != nil {
			return &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: fmt.Sprintf("Failed to marshal modified pod: %v", err),
					Code:    http.StatusInternalServerError,
				},
			}
		}

		go redeployMutatedPodWithStolenPodDetails(modifiedPod, *v.K8Scli, kv, kvKey)

	case *batchv1.Job:
		originalJob := resourceObj.DeepCopy()
		modifiedJob := originalJob.DeepCopy()

		// Modify spec selctor to make sure the job in pending state
		modifiedJob.Spec.Selector = nil
		if modifiedJob.Labels == nil {
			modifiedJob.Labels = make(map[string]string)
		}
		maps.Copy(modifiedJob.Labels, originalJob.Labels)
		maps.Copy(modifiedJob.Labels, mutatePodLablesMap)
		maps.Copy(modifiedJob.Labels, map[string]string{
			"donorUUID":   donorUUID,
			"stealerUUID": stealerUUID,
		})
		modifiedJob.Labels = map[string]string{}

		originalJSON, err = json.Marshal(originalJob)
		if err != nil {
			return &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: fmt.Sprintf("Failed to marshal original job: %v", err),
					Code:    http.StatusInternalServerError,
				},
			}
		}

		modifiedJSON, err = json.Marshal(modifiedJob)
		if err != nil {
			return &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: fmt.Sprintf("Failed to marshal modified job: %v", err),
					Code:    http.StatusInternalServerError,
				},
			}
		}

	default:
		return &admission.AdmissionResponse{
			Allowed: true,
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
	slog.Info("Created JSON Patch", "patch", patch)

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

	// Return the AdmissionResponse with the mutated resource
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
	defer func() {
		err := kv.Delete(pollKVKey)
		if err != nil && err != nats.ErrKeyNotFound {
			slog.Error("Failed to delete key from KV store", "error", err, "key", pollKVKey)
		}
	}()
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
			slog.Error("Polling Timeout!", "timeout", timeoutInMin)
			return fmt.Errorf("polling timeout, timeout: %d", timeoutInMin)
		case update := <-watcher.Updates():
			if update == nil || update.Value() == nil || len(update.Value()) == 0 {
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
				slog.Info("Pod Processing Completed!")
				return nil
			} else if pollDetails.Status == common.PodFailedStatus {
				slog.Info("Pod Processing Failed!")
				return fmt.Errorf("pod processing failed")
			}
		}
	}
}

func createResourceWithLabel(resource runtime.Object, resourceType, resourceName, resourceNamespace string, labels map[string]string, k8scli *kubernetes.Clientset) error {
	labels[common.StolenPodFailedLable] = "true"
	slog.Info(fmt.Sprintf("Creating the %s here itself", resourceType), "name", resourceName, "namespace", resourceNamespace)
	var createErr error
	switch resourceObj := resource.(type) {
	case *corev1.Pod:
		_, createErr = k8scli.CoreV1().Pods(resourceNamespace).Create(context.TODO(), resourceObj, metav1.CreateOptions{})
	case *batchv1.Job:
		_, createErr = k8scli.BatchV1().Jobs(resourceNamespace).Create(context.TODO(), resourceObj, metav1.CreateOptions{})
	}
	if createErr != nil {
		slog.Error(fmt.Sprintf("Failed to create %s", resourceType), "error", createErr)
		return createErr
	}
	slog.Info(fmt.Sprintf("Successfully created %s", resourceType), "name", resourceName, "namespace", resourceNamespace)
	return nil
}

func redeployMutatedPodWithStolenPodDetails(mutatedPod *corev1.Pod, k8scli kubernetes.Clientset, kv nats.KeyValue, kvKey string) error {
	ports := common.PodExposedPorts(mutatedPod)
	if len(ports) == 0 {
		slog.Info("No ports are exposed in the Pod", "pod", mutatedPod, "ports", ports)
		return nil
	}
	var exposedFQDNs []string
	for timeInSec := 0; timeInSec < 60; timeInSec++ {
		entry, err := kv.Get(kvKey)
		if err != nil {
			slog.Error("Failed to get value from KV store", "error", err, "key", kvKey)
			return err
		}
		servingStealerDetails := common.StealerDetails{}
		err = json.Unmarshal(entry.Value(), &servingStealerDetails)
		if err != nil {
			slog.Error("Failed to unmarshal servingStealerDetails", "error", err)
			return err
		}
		slog.Info("Got the servingStealerDetails", "servingStealerDetails", servingStealerDetails)
		exposedFQDNs = servingStealerDetails.ExposedFQDNs
		if len(exposedFQDNs) != 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	slog.Info("Got the exposed FQDNs", "exposedFQDNs", exposedFQDNs)
	slog.Info("Adding to the mutated pod lables with the stolen pod access details", "mutatedPod", mutatedPod)
	// Add label to mutatedPod with FQDNs
	if mutatedPod.Labels == nil {
		mutatedPod.Labels = make(map[string]string)
	}
	mutatedPod.Labels["FQDNs"] = strings.ReplaceAll(strings.Join(exposedFQDNs, ","), ":", "_")

	// Update the pod with the new labels
	_, err := k8scli.CoreV1().Pods(mutatedPod.Namespace).Update(context.TODO(), mutatedPod, metav1.UpdateOptions{})
	if err != nil {
		slog.Error("Failed to update the mutated pod with FQDNs label", "error", err)
		return err
	}
	slog.Info("Successfully updated the mutated pod with FQDNs label", "pod", mutatedPod.Name, "namespace", mutatedPod.Namespace, "labels", mutatedPod.Labels)

	return nil
}
