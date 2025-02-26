package worker

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ExportService(K8SConfig *rest.Config, service *corev1.Service) (*ServiceExport, error) {
	// Create a new scheme and register the Submariner types
	sch := runtime.NewScheme()
	err := scheme.AddToScheme(sch)
	if err != nil {
		slog.Error("Failed to add Submariner types to scheme", "error", err)
	}

	// Add the Submariner ServiceExport type to the scheme
	err = AddToScheme(sch)
	if err != nil {
		slog.Error("Failed to add Submariner ServiceExport type to scheme", "error", err)
	}

	// Create a Kubernetes client
	k8sClient, err := client.New(K8SConfig, client.Options{Scheme: sch})
	if err != nil {
		slog.Error("Error creating Kubernetes client", "error", err)
		return nil, err
	}

	// Define the ServiceExport object
	serviceExport := &ServiceExport{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "multicluster.x-k8s.io/v1alpha1",
			Kind:       "ServiceExport",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
		},
		Spec: ServiceExportSpec{},
	}

	// Create the ServiceExport in the cluster
	err = k8sClient.Create(context.TODO(), serviceExport)
	if err != nil {
		slog.Error("Failed to create ServiceExport", "error", err)
		return nil, err
	}
	return serviceExport, nil
}

// AddToScheme adds the Submariner types to the scheme
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(
		GroupVersion,
		&ServiceExport{},
		&ServiceExportList{},
	)
	// Add the GroupVersion to the scheme
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

// GroupVersion is the group and version for Submariner resources
var GroupVersion = schema.GroupVersion{Group: "multicluster.x-k8s.io", Version: "v1alpha1"}

// ServiceExport represents the ServiceExport resource in Submariner
type ServiceExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServiceExportSpec `json:"spec,omitempty"`
}

// DeepCopyObject implements the runtime.Object interface
func (in *ServiceExport) DeepCopyObject() runtime.Object {
	out := &ServiceExport{}
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into the given *ServiceExport
func (in *ServiceExport) DeepCopyInto(out *ServiceExport) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

// ServiceExportSpec defines the desired state of ServiceExport
type ServiceExportSpec struct {
	// Add any specific fields for ServiceExport here
}

// ServiceExportList is a list of ServiceExport resources
type ServiceExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceExport `json:"items"`
}

// DeepCopyObject implements the runtime.Object interface
func (in *ServiceExportList) DeepCopyObject() runtime.Object {
	out := &ServiceExportList{}
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into the given *ServiceExportList
func (in *ServiceExportList) DeepCopyInto(out *ServiceExportList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]ServiceExport, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}
