package donor

import (
	natsconnect "hirocrossclusterworkloads/pkg/connector/nats"

	admission "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
)

type DWConfig struct {
	Nclient   natsconnect.NATSClient
	DonorUUID string
}

type DVConfig struct {
	Nconfig          natsconnect.NATSClient
	DonorUUID        string
	LableToFilter    string
	IgnoreNamespaces []string
}

// Create a struct with donorUUID and pod object
type DonorPod struct {
	DonorUUID string      `json:"donorUUID"`
	Pod       *corev1.Pod `json:"pod"`
}

type Validator interface {
	Validate(admission.AdmissionReview) *admission.AdmissionResponse
	Mutate(admission.AdmissionReview) *admission.AdmissionResponse
}

type DSConfig struct {
	MPort       int
	VPort       int
	TLSKeyPath  string
	TLSCertPath string
}

type DServer interface {
	StartValidate(stopChan chan<- bool) error
	StartMutate(stopChan chan<- bool) error
}
