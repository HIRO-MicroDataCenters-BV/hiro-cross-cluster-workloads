package common

import (
	"context"
	"log/slog"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	StolenPodFailedLable = "StolenOneFailed"
	K8SNamespaces        = []string{"default", "kube-system", "kube-public",
		"kube-node-lease", "kube-admission", "kube-proxy", "kube-controller-manager",
		"kube-scheduler", "kube-dns"}
	DonorKVValuePending = "Pending"
	PodFinishedStatus   = "Successful"
	PodFailedStatus     = "Failed"
)

type DonorDetails struct {
	DonorUUID string `json:"donorUUID"`
	KVKey     string `json:"kvKey"`
	WaitTime  int    `json:"waitTime"`
}

type StealerDetails struct {
	StealerUUID  string   `json:"stealerUUID"`
	KVKey        string   `json:"kvKey"`
	ExposedFQDNs []string `json:"exposedFQDNs"`
}

type DonorPod struct {
	DonorDetails DonorDetails `json:"donorDetails"`
	Pod          *corev1.Pod  `json:"pod"`
}

type DonorJob struct {
	DonorDetails DonorDetails `json:"donorDetails"`
	Job          *batchv1.Job `json:"job"`
}

type DonorDeployment struct {
	DonorDetails DonorDetails       `json:"donorDetails"`
	Deployment   *appsv1.Deployment `json:"deployment"`
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

func GetK8sClientAndConfigSet() (*kubernetes.Clientset, *rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	return clientset, config, err
}

func GetClusterID() (string, error) {
	clientset, _, err := GetK8sClientAndConfigSet()
	if err != nil {
		return "", err
	}
	nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(nodeList.Items) == 0 {
		return "", nil
	}
	return nodeList.Items[0].Status.NodeInfo.SystemUUID, nil
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

func IsLabelExists(obj metav1.Object, label string) bool {
	value, ok := obj.GetLabels()[label]
	if !ok || strings.ToLower(value) == "false" {
		return false
	}
	return true
}

func GenerateServiceNameForStolenPod(podName string) string {
	return podName + "-service"
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

func ResourceExposedPorts(resource runtime.Object) []corev1.ServicePort {
	var ports []corev1.ServicePort

	switch r := resource.(type) {
	case *corev1.Pod:
		for _, container := range r.Spec.Containers {
			for _, port := range container.Ports {
				ports = append(ports, corev1.ServicePort{
					Name:       port.Name,
					Protocol:   port.Protocol,
					Port:       port.ContainerPort,
					TargetPort: intstr.FromInt(int(port.ContainerPort)),
				})
			}
		}
	case *appsv1.Deployment:
		for _, container := range r.Spec.Template.Spec.Containers {
			for _, port := range container.Ports {
				ports = append(ports, corev1.ServicePort{
					Name:       port.Name,
					Protocol:   port.Protocol,
					Port:       port.ContainerPort,
					TargetPort: intstr.FromInt(int(port.ContainerPort)),
				})
			}
		}
	default:
		slog.Warn("Unsupported resource type", "resource", resource)
	}

	slog.Info("Exposed Ports", "ports", ports)
	return ports
}
