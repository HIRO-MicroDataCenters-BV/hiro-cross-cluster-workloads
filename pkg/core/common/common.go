package common

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecFactory  = serializer.NewCodecFactory(runtimeScheme)
	Deserializer  = codecFactory.UniversalDeserializer()
	// JetStreamQueue     = "worker-group" //should be same for all workers/stealers
	StolenPodLablesMap = map[string]string{
		"is-pod-stolen": "true",
		"donorUUID":     "",
		"stealerUUID":   "",
	}
	StolenPodFailedLable = "StealFailed"
	K8SNamespaces        = []string{"default", "kube-system", "kube-public",
		"kube-node-lease", "kube-admission", "kube-proxy", "kube-controller-manager",
		"kube-scheduler", "kube-dns"}
	DonorKVValuePending = "Pending"
	PodFinishedStatus   = "Successful"
	PodFailedStatus     = "Failed"
)

// Create a struct with donorUUID and pod object
type DonorPod struct {
	KVKey     string      `json:"kvKey"`
	DonorUUID string      `json:"donorUUID"`
	Pod       *corev1.Pod `json:"pod"`
	WaitTime  int         `json:"waitTime"`
}

type Result struct {
	StealerUUID    string      `json:"stealerUUID"`
	DonorUUID      string      `json:"donorUUID"`
	Message        string      `json:"message"`
	Status         string      `json:"status"`
	StartTime      metav1.Time `json:"startTime"`
	CompletionTime metav1.Time `json:"completionTime"`
	Duration       string      `json:"duration"`
}

type PodResults struct {
	Results Result      `json:"results"`
	Pod     *corev1.Pod `json:"pod"`
}

type PodPollDetails struct {
	Status    string `json:"status"`
	Duration  string `json:"duration"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func GetK8sClientSet() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, err
}

func MergeMaps(map1, map2 map[string]string) map[string]string {
	merged := make(map[string]string)
	for k, v := range map1 {
		merged[k] = v
	}
	for k, v := range map2 {
		merged[k] = v
	}
	return merged
}

func IsItIgnoredNamespace(list []string, item string) bool {
	for _, str := range list {
		if str == item {
			return true
		}
	}
	return false
}

func MergeUnique(slice1, slice2 []string) []string {
	uniqueMap := make(map[string]bool)
	result := []string{}

	for _, item := range slice1 {
		uniqueMap[item] = true
	}
	for _, item := range slice2 {
		uniqueMap[item] = true
	}

	for key := range uniqueMap {
		result = append(result, key)
	}

	return result
}

func AreMapsEqual(map1, map2 map[string]string) bool {
	if len(map1) != len(map2) {
		return false
	}
	for k, v := range map1 {
		if map2[k] != v {
			return false
		}
	}
	return true
}

func IsLableExists(pod *corev1.Pod, lable string) bool {
	value, ok := pod.Labels[lable]
	if !ok || strings.ToLower(value) == "false" {
		return false
	}
	return true
}

func GenerateKVKey(input ...string) string {
	return strings.Join(input, ".")
}

func GenerateStealWorkloadKVKey(donorUUID, namespace, podName string) string {
	return GenerateKVKey(donorUUID, namespace, podName)
}

func GeneratePollStealWorkloadKVKey(donorUUID, stealerUUID, namespace, podName string) string {
	return GenerateKVKey(donorUUID, stealerUUID, namespace, podName)
}
