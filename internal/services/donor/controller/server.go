package controller

import (
	"encoding/json"
	"fmt"
	"hirocrossclusterworkloads/pkg/core/donor"
	"io"
	"log/slog"
	"net/http"

	admission "k8s.io/api/admission/v1"
	admissionregistration "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecFactory  = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecFactory.UniversalDeserializer()
)

type server struct {
	maddress  string
	vaddress  string
	config    donor.DSConfig
	validator donor.Validator
}

func New(config donor.DSConfig, validator donor.Validator) donor.DServer {
	vaddr := fmt.Sprintf(":%d", config.VPort)
	maddr := fmt.Sprintf(":%d", config.MPort)
	return &server{
		maddress:  maddr,
		vaddress:  vaddr,
		config:    config,
		validator: validator,
	}
}

func (s *server) StartValidate(stopChan chan<- bool) error {
	http.HandleFunc("/validate", s.validateHandler)
	slog.Info("Validation Admission Webhook Server started ...")

	err := http.ListenAndServeTLS(s.vaddress, s.config.TLSCertPath, s.config.TLSKeyPath, nil)
	if err != nil {
		slog.Error("StartValidate: Server exited with error:", "error", err)
		stopChan <- true // Send signal only on error
	}
	return err
}

func (s *server) StartMutate(stopChan chan<- bool) error {
	http.HandleFunc("/mutate", s.mutateHandler)
	slog.Info("Mutation Admission Webhook Server started ...")

	err := http.ListenAndServeTLS(s.maddress, s.config.TLSCertPath, s.config.TLSKeyPath, nil)
	if err != nil {
		slog.Error("StartValidate: Server exited with error:", "error", err)
		stopChan <- true // Send signal only on error
	}
	return err
}

func (s *server) validateHandler(w http.ResponseWriter, r *http.Request) {
	slog.Info("Validating Request")

	admission.AddToScheme(runtimeScheme)
	admissionregistration.AddToScheme(runtimeScheme)

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		slog.Error("error: unexpected content type, expected application/json", "content-type", contentType)
		msg := fmt.Sprintf("contentType=%s, expect application/json", contentType)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	if r.Body == nil {
		slog.Error("Request with no body")
		http.Error(w, "Request with no body", http.StatusBadRequest)
		return
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Unable to read request data")
		http.Error(w, "Request with unreadable body", http.StatusBadRequest)
		return
	}

	slog.Info(fmt.Sprintf("handling request: %s", data))
	admissionReview := &admission.AdmissionReview{}
	obj, gvk, err := deserializer.Decode(data, nil, admissionReview)
	if err != nil {
		slog.Error("Request could not be decoded", "error", err)
		msg := fmt.Sprintf("Request could not be decoded: %v", err)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	requestedAdmissionReview, ok := obj.(*admission.AdmissionReview)
	if !ok {
		slog.Error("unexpected object type, expected v1.AdmissionReview", "object", obj)
		msg := fmt.Sprintf("unexpected object type, expected v1.AdmissionReview but got: %T", obj)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	responseAdmissionReview := &admission.AdmissionReview{}
	responseAdmissionReview.SetGroupVersionKind(*gvk)
	responseAdmissionReview.Response = s.validator.Validate(*requestedAdmissionReview)
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
	responseObj := responseAdmissionReview

	slog.Info("sending response", "response", responseObj)
	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		slog.Error("response marshal failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		slog.Error("failed to write response", "error", err)
	}
}

func (s *server) mutateHandler(w http.ResponseWriter, r *http.Request) {
	slog.Info("Validating Request")

	admission.AddToScheme(runtimeScheme)
	admissionregistration.AddToScheme(runtimeScheme)

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		slog.Error("error: unexpected content type, expected application/json", "content-type", contentType)
		msg := fmt.Sprintf("contentType=%s, expect application/json", contentType)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	if r.Body == nil {
		slog.Error("Request with no body")
		http.Error(w, "Request with no body", http.StatusBadRequest)
		return
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Unable to read request data")
		http.Error(w, "Request with unreadable body", http.StatusBadRequest)
		return
	}

	slog.Info("Handling request", "data", data)
	admissionReview := &admission.AdmissionReview{}
	obj, gvk, err := deserializer.Decode(data, nil, admissionReview)
	if err != nil {
		slog.Error("Request could not be decoded", "error", err)
		msg := fmt.Sprintf("Request could not be decoded: %v", err)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	requestedAdmissionReview, ok := obj.(*admission.AdmissionReview)
	if !ok {
		slog.Error("unexpected object type, expected v1.AdmissionReview", "object", obj)
		msg := fmt.Sprintf("unexpected object type, expected v1.AdmissionReview but got: %T", obj)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	responseAdmissionReview := &admission.AdmissionReview{}
	responseAdmissionReview.SetGroupVersionKind(*gvk)
	responseAdmissionReview.Response = s.validator.Mutate(*requestedAdmissionReview)
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
	responseObj := responseAdmissionReview

	slog.Info("sending response", "response", responseObj)
	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		slog.Error("response marshal failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		slog.Error("failed to write response", "error", err)
	}
}
