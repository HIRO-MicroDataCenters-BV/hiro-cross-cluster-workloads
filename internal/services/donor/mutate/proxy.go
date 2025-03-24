package mutate

import (
	"context"
	"fmt"
	"hirocrossclusterworkloads/pkg/core/common"
	"log/slog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	appLabelsMap = map[string]string{"app": "nginx-proxy"}
)

func int32Ptr(i int32) *int32 {
	return &i
}

func CreateProxyServiceAsRevereseProxy(K8Scli kubernetes.Clientset, name, namespace string, labelsMap map[string]string, exposedFQDN string, exposedPorts []corev1.ServicePort, replicas int) (*corev1.Service, error) {
	// Step 1: Create ConfigMap
	labelsMap = common.MergeMaps(labelsMap, appLabelsMap)
	configMap := generateNginxConfigMap(name, namespace, exposedFQDN, exposedPorts, labelsMap)
	createConfigMap, err := K8Scli.CoreV1().ConfigMaps(namespace).Create(context.TODO(), configMap, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create ConfigMap", "error", err)
		return nil, err
	}
	slog.Info("ConfigMap created successfully!", "configMap", configMap)

	deployment := generateNginxDeployment(name, namespace, createConfigMap.Name, replicas, labelsMap)
	_, err = K8Scli.AppsV1().Deployments(namespace).Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create Deployment", "error", err)
		return nil, err
	}
	slog.Info("Deployment created successfully!", "deployment", deployment)

	service := genereateNginxService(name, namespace, exposedPorts, labelsMap)
	_, err = K8Scli.CoreV1().Services(namespace).Create(context.TODO(), service, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create Service", "error", err)
	}
	fmt.Println("Service created successfully!", "service", service)
	return service, nil
}

func generateNginxConfigMap(name, namespace, exposedFQDN string, exposedPorts []corev1.ServicePort, labelsMap map[string]string) *corev1.ConfigMap {
	// Step 1: Create ConfigMap
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-nginx-proxy-configmap",
			Namespace: namespace,
			Labels:    labelsMap,
		},
		Data: map[string]string{
			"nginx.conf": generateNginxConfig(exposedFQDN, exposedPorts),
		},
	}
}

func generateNginxConfig(exposedFQDN string, exposedPorts []corev1.ServicePort) string {
	config := "events {}\n\nhttp {\n"
	for _, port := range exposedPorts {
		config += fmt.Sprintf("server {\n\tlisten %d;\n\tlocation / {\n\t\tproxy_pass http://%s:%d;\n\t}\n}\n", port.TargetPort.IntValue(), exposedFQDN, port.TargetPort.IntValue())
	}
	config += "}\n"
	return config
}

func generateNginxDeployment(name, namespace, configMapName string, replicas int, labelsMap map[string]string) *appsv1.Deployment {

	// Step 2: Create Deployment
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-nginx-proxy-deployment",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(int32(replicas)),
			Selector: &metav1.LabelSelector{
				MatchLabels: labelsMap,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelsMap,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:latest",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config-volume",
									MountPath: "/etc/nginx/nginx.conf",
									SubPath:   "nginx.conf",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
								},
							},
						},
					},
				},
			},
		},
	}
	return deployment
}

func genereateNginxService(name, namespace string, exposedPorts []corev1.ServicePort, labelsMap map[string]string) *corev1.Service {
	// Step 3: Create Service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-nginx-proxy-service",
			Namespace: namespace,
			Labels:    labelsMap,
		},
		Spec: corev1.ServiceSpec{
			Selector: labelsMap,
			Ports:    exposedPorts,
		},
	}
	return service
}

func CreateProxyServiceWithHeadLessType(k8scli kubernetes.Clientset, name, namespace string, labelsMap map[string]string, exposedFQDN string) (*corev1.Service, error) {
	labelsMap = common.MergeMaps(labelsMap, appLabelsMap)
	proxyService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-service-proxy", name),
			Namespace: namespace,
			Labels:    labelsMap,
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: exposedFQDN,
		},
	}

	createdService, err := k8scli.CoreV1().Services(namespace).Create(context.TODO(), proxyService, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create proxy service", "error", err)
		return nil, err
	}
	slog.Info("Successfully created proxy service", "service", createdService.Name, "namespace", createdService.Namespace)
	return createdService, nil
}
