package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AdmissionHandler handles incoming admission review requests for pod eviction.
type AdmissionHandler struct {
	kubeClient   client.Client
	eppClient    *EPPClient
	podSelector  *PodSelector
}

// NewAdmissionHandler creates a new admission webhook handler.
func NewAdmissionHandler(kubeClient client.Client, eppClient *EPPClient, namespace string) *AdmissionHandler {
	return &AdmissionHandler{
		kubeClient:  kubeClient,
		eppClient:   eppClient,
		podSelector: NewPodSelector(kubeClient, eppClient, namespace),
	}
}

// Handle processes an admission review request.
func (h *AdmissionHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	// Parse admission review
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		logger.Error(err, "failed to decode admission review")
		h.writeResponse(w, admissionReview(review.Request.UID, false, "invalid request"))
		return
	}

	logger.V(1).Info("Received admission review", "kind", review.Request.Kind, "name", review.Request.Name)

	// Check if this is an eviction request
	if !isEvictionRequest(&review) {
		logger.V(1).Info("Not an eviction request, admitting")
		h.writeResponse(w, admissionReview(review.Request.UID, true, "not an eviction"))
		return
	}

	logger.Info("Eviction request intercepted", "pod", review.Request.Name, "namespace", review.Request.Namespace)

	// Update pod deletion costs before admitting
	if err := h.podSelector.UpdatePodDeletionCosts(ctx); err != nil {
		logger.Error(err, "failed to update pod deletion costs")
		// Fail open: admit the eviction anyway
		h.writeResponse(w, admissionReview(review.Request.UID, true, fmt.Sprintf("error: %v", err)))
		return
	}

	logger.Info("Pod deletion costs updated, admitting eviction")
	h.writeResponse(w, admissionReview(review.Request.UID, true, "pod costs updated"))
}

// isEvictionRequest checks if the admission review is for a pod eviction.
func isEvictionRequest(review *admissionv1.AdmissionReview) bool {
	if review.Request == nil {
		return false
	}
	return review.Request.Kind.Kind == "Pod" &&
		review.Request.Kind.Group == "" &&
		review.Request.Resource.Resource == "pods" &&
		review.Request.SubResource == "eviction"
}

// admissionReview creates an admission review response.
func admissionReview(uid types.UID, allowed bool, message string) *admissionv1.AdmissionReview {
	return &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     uid,
			Allowed: allowed,
			Result: &metav1.Status{
				Code:    200,
				Message: message,
			},
		},
	}
}

// writeResponse writes the admission review response to the client.
func (h *AdmissionHandler) writeResponse(w http.ResponseWriter, review *admissionv1.AdmissionReview) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(review)
}
