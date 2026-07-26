package webhook

import (
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestIsEvictionRequest(t *testing.T) {
	tests := []struct {
		name     string
		review   *admissionv1.AdmissionReview
		expected bool
	}{
		{
			name: "eviction request",
			review: &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					Kind: metav1.GroupVersionKind{
						Group: "",
						Kind:  "Pod",
					},
					Resource: metav1.GroupVersionResource{
						Resource: "pods",
					},
					SubResource: "eviction",
				},
			},
			expected: true,
		},
		{
			name: "non-eviction request",
			review: &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					Kind: metav1.GroupVersionKind{
						Group: "",
						Kind:  "Pod",
					},
					Resource: metav1.GroupVersionResource{
						Resource: "pods",
					},
					SubResource: "",
				},
			},
			expected: false,
		},
		{
			name: "nil request",
			review: &admissionv1.AdmissionReview{
				Request: nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isEvictionRequest(tt.review)
			if got != tt.expected {
				t.Errorf("isEvictionRequest() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestAdmissionReviewResponse(t *testing.T) {
	uid := "test-uid"
	msg := "test message"
	response := admissionReview(types.UID(uid), true, msg)

	if response.Response.UID != types.UID(uid) {
		t.Errorf("UID mismatch: got %s, want %s", response.Response.UID, uid)
	}

	if !response.Response.Allowed {
		t.Errorf("Response should be allowed")
	}

	if response.Response.Result.Message != msg {
		t.Errorf("Message mismatch: got %s, want %s", response.Response.Result.Message, msg)
	}
}

func TestHandlerNonEviction(t *testing.T) {
	// Skip test - requires full Kubernetes environment
	// See integration tests for full handler testing
	t.Skip("Handler test skipped - see integration tests")
}
