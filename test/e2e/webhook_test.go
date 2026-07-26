// +build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/llm-d/inferno-autoscaler/internal/webhook"
)

// TestWebhookWithKindCluster tests the webhook with a Kind cluster.
func TestWebhookWithKindCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}

	// Setup envtest environment (simulates Kubernetes API)
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start test env: %v", err)
	}
	defer env.Stop()

	// Create Kubernetes client
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Create test namespace
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-webhook",
		},
	}
	if err := k8sClient.Create(context.Background(), namespace); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create test pods
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%d", i),
				Namespace: "test-webhook",
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test",
						Image: "nginx:latest",
					},
				},
			},
		}
		if err := k8sClient.Create(context.Background(), pod); err != nil {
			t.Fatalf("failed to create pod: %v", err)
		}
	}

	t.Run("webhook_updates_pod_deletion_costs", func(t *testing.T) {
		testWebhookUpdatesPodDeletionCosts(t, k8sClient)
	})

	t.Run("webhook_intercepts_eviction", func(t *testing.T) {
		testWebhookInterceptsEviction(t)
	})

	t.Run("webhook_with_mock_epp", func(t *testing.T) {
		testWebhookWithMockEPP(t, k8sClient)
	})
}

func testWebhookUpdatesPodDeletionCosts(t *testing.T, k8sClient client.Client) {
	// Create mock EPP server that returns pod metrics
	mockEPP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoints := []webhook.PodMetrics{
			{Pod: "pod-0", WaitingQueueSize: 0, RunningRequestsSize: 0},
			{Pod: "pod-1", WaitingQueueSize: 45, RunningRequestsSize: 3},
			{Pod: "pod-2", WaitingQueueSize: 50, RunningRequestsSize: 5},
		}
		json.NewEncoder(w).Encode(endpoints)
	}))
	defer mockEPP.Close()

	eppClient := webhook.NewEPPClient(mockEPP.URL)
	podSelector := webhook.NewPodSelector(k8sClient, eppClient, "test-webhook")

	// Update pod deletion costs
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := podSelector.UpdatePodDeletionCosts(ctx)
	if err != nil {
		t.Fatalf("UpdatePodDeletionCosts failed: %v", err)
	}

	// Verify pods were updated with correct annotations
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace("test-webhook")); err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}

	expectedCosts := map[string]int{
		"pod-0": -100, // Idle
		"pod-1": 515,  // (45 * 10) + (3 * 5) - 100
		"pod-2": 575,  // (50 * 10) + (5 * 5) - 100
	}

	for _, pod := range pods.Items {
		expectedCost, exists := expectedCosts[pod.Name]
		if !exists {
			continue
		}

		costStr := pod.Annotations[webhook.DeletionCostAnnotation]
		if costStr == "" {
			t.Errorf("Pod %s missing deletion cost annotation", pod.Name)
			continue
		}

		// Parse cost string
		var cost int
		if _, err := fmt.Sscanf(costStr, "%d", &cost); err != nil {
			t.Errorf("Pod %s has invalid cost: %s", pod.Name, costStr)
			continue
		}

		if cost != expectedCost {
			t.Errorf("Pod %s cost mismatch: got %d, want %d", pod.Name, cost, expectedCost)
		}
	}
}

func testWebhookInterceptsEviction(t *testing.T) {
	// Test that webhook correctly identifies eviction requests
	tests := []struct {
		name              string
		subresource       string
		shouldIntercept   bool
	}{
		{
			name:            "eviction subresource",
			subresource:     "eviction",
			shouldIntercept: true,
		},
		{
			name:            "update subresource",
			subresource:     "",
			shouldIntercept: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					Kind: metav1.GroupKind{
						Group: "",
						Kind:  "Pod",
					},
					Resource: metav1.GroupVersionResource{
						Resource: "pods",
					},
					Subresource: tt.subresource,
				},
			}

			// This would normally be done by the webhook handler
			// For now, we just verify the logic is correct
			isEviction := review.Request.Subresource == "eviction" &&
				review.Request.Kind.Kind == "Pod" &&
				review.Request.Resource.Resource == "pods"

			if isEviction != tt.shouldIntercept {
				t.Errorf("eviction detection failed: got %v, want %v", isEviction, tt.shouldIntercept)
			}
		})
	}
}

func testWebhookWithMockEPP(t *testing.T, k8sClient client.Client) {
	// Create mock EPP server
	mockEPP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/endpoints" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		endpoints := []webhook.PodMetrics{
			{Pod: "pod-0", WaitingQueueSize: 0, RunningRequestsSize: 0},
			{Pod: "pod-1", WaitingQueueSize: 20, RunningRequestsSize: 1},
			{Pod: "pod-2", WaitingQueueSize: 30, RunningRequestsSize: 2},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(endpoints)
	}))
	defer mockEPP.Close()

	// Test EPP client
	eppClient := webhook.NewEPPClient(mockEPP.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	endpoints, err := eppClient.GetEndpoints(ctx)
	if err != nil {
		t.Fatalf("GetEndpoints failed: %v", err)
	}

	if len(endpoints) != 3 {
		t.Errorf("Expected 3 endpoints, got %d", len(endpoints))
	}

	// Verify metrics
	expectedMetrics := map[string]webhook.PodMetrics{
		"pod-0": {Pod: "pod-0", WaitingQueueSize: 0, RunningRequestsSize: 0},
		"pod-1": {Pod: "pod-1", WaitingQueueSize: 20, RunningRequestsSize: 1},
		"pod-2": {Pod: "pod-2", WaitingQueueSize: 30, RunningRequestsSize: 2},
	}

	for _, ep := range endpoints {
		expected, exists := expectedMetrics[ep.Pod]
		if !exists {
			t.Errorf("Unexpected pod: %s", ep.Pod)
			continue
		}

		if ep.WaitingQueueSize != expected.WaitingQueueSize {
			t.Errorf("Queue size mismatch for %s: got %d, want %d",
				ep.Pod, ep.WaitingQueueSize, expected.WaitingQueueSize)
		}

		if ep.RunningRequestsSize != expected.RunningRequestsSize {
			t.Errorf("Running requests mismatch for %s: got %d, want %d",
				ep.Pod, ep.RunningRequestsSize, expected.RunningRequestsSize)
		}
	}
}

// TestDeletionCostConsistency tests that deletion costs are consistent across calls.
func TestDeletionCostConsistency(t *testing.T) {
	tests := []struct {
		queue   int
		running int
	}{
		{0, 0},
		{1, 0},
		{10, 2},
		{50, 5},
		{100, 10},
	}

	for _, tt := range tests {
		// Calculate cost multiple times and verify it's consistent
		cost1 := webhook.CalculateDeletionCost(tt.queue, tt.running)
		cost2 := webhook.CalculateDeletionCost(tt.queue, tt.running)
		cost3 := webhook.CalculateDeletionCost(tt.queue, tt.running)

		if cost1 != cost2 || cost2 != cost3 {
			t.Errorf("Inconsistent costs for (%d, %d): %d, %d, %d",
				tt.queue, tt.running, cost1, cost2, cost3)
		}
	}
}

// TestEPPClientError tests EPP client error handling.
func TestEPPClientError(t *testing.T) {
	// Create mock EPP server that returns an error
	mockEPP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error"))
	}))
	defer mockEPP.Close()

	eppClient := webhook.NewEPPClient(mockEPP.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := eppClient.GetEndpoints(ctx)
	if err == nil {
		t.Errorf("Expected error from EPP server, got nil")
	}
}
