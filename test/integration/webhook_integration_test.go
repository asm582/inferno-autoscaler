// +build integration

package integration

import (
	"bytes"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/llm-d/inferno-autoscaler/internal/webhook"
)

// TestWebhookIntegration tests the complete webhook flow with Kubernetes API and EPP.
func TestWebhookIntegration(t *testing.T) {
	// Start test Kubernetes environment
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer env.Stop()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("failed to create k8s client: %v", err)
	}

	// Create test namespace
	ctx := context.Background()
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-integration",
		},
	}
	if err := k8sClient.Create(ctx, namespace); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Mock EPP server
	mockEPPServer := setupMockEPPServer(t)
	defer mockEPPServer.Close()

	eppClient := webhook.NewEPPClient(mockEPPServer.URL)

	t.Run("scenario_scale_down_evicts_idle_pod", func(t *testing.T) {
		testScaleDownEvictsIdlePod(t, ctx, k8sClient, eppClient)
	})

	t.Run("scenario_scale_down_protects_loaded_pods", func(t *testing.T) {
		testScaleDownProtectsLoadedPods(t, ctx, k8sClient, eppClient)
	})

	t.Run("scenario_webhook_admission_flow", func(t *testing.T) {
		testWebhookAdmissionFlow(t, ctx, k8sClient, eppClient)
	})

	t.Run("scenario_epp_failure_fallback", func(t *testing.T) {
		testEPPFailureFallback(t, ctx, k8sClient)
	})
}

func setupMockEPPServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Mock /endpoints endpoint
	mux.HandleFunc("/endpoints", func(w http.ResponseWriter, r *http.Request) {
		endpoints := []webhook.PodMetrics{
			{Pod: "inference-pod-0", WaitingQueueSize: 0, RunningRequestsSize: 0},
			{Pod: "inference-pod-1", WaitingQueueSize: 45, RunningRequestsSize: 3},
			{Pod: "inference-pod-2", WaitingQueueSize: 50, RunningRequestsSize: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(endpoints)
	})

	// Mock /health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	return httptest.NewServer(mux)
}

// testScaleDownEvictsIdlePod tests that during scale-down, idle pods (queue=0) are selected first.
func testScaleDownEvictsIdlePod(t *testing.T, ctx context.Context, k8sClient client.Client, eppClient *webhook.EPPClient) {
	ns := "test-integration"

	// Create 3 test pods
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("inference-pod-%d", i),
				Namespace: ns,
				Labels: map[string]string{
					"app": "inference",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "inference",
						Image: "vllm:latest",
					},
				},
			},
		}
		if err := k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("failed to create pod: %v", err)
		}
	}

	// Create pod selector
	podSelector := webhook.NewPodSelector(k8sClient, eppClient, ns)

	// Update pod deletion costs (simulating scale-down event)
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := podSelector.UpdatePodDeletionCosts(timeoutCtx); err != nil {
		t.Fatalf("UpdatePodDeletionCosts failed: %v", err)
	}

	// Verify that costs were set correctly
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}

	// Find the costs
	costs := make(map[string]int)
	for _, pod := range pods.Items {
		if pod.Annotations == nil {
			continue
		}
		costStr := pod.Annotations[webhook.DeletionCostAnnotation]
		if costStr != "" {
			var cost int
			fmt.Sscanf(costStr, "%d", &cost)
			costs[pod.Name] = cost
		}
	}

	// Verify idle pod has lowest cost
	idlePodCost := costs["inference-pod-0"]
	loadedPod1Cost := costs["inference-pod-1"]
	loadedPod2Cost := costs["inference-pod-2"]

	if idlePodCost >= loadedPod1Cost || idlePodCost >= loadedPod2Cost {
		t.Errorf("Idle pod cost should be lower: idle=%d, loaded1=%d, loaded2=%d",
			idlePodCost, loadedPod1Cost, loadedPod2Cost)
	}

	// Verify specific expected values
	if idlePodCost != -100 {
		t.Errorf("Expected idle pod cost -100, got %d", idlePodCost)
	}

	if loadedPod1Cost != 515 {
		t.Errorf("Expected pod-1 cost 515, got %d", loadedPod1Cost)
	}

	if loadedPod2Cost != 575 {
		t.Errorf("Expected pod-2 cost 575, got %d", loadedPod2Cost)
	}

	t.Logf("✓ Scale-down would evict idle pod (cost %d) before loaded pods", idlePodCost)
}

// testScaleDownProtectsLoadedPods verifies that loaded pods have higher costs.
func testScaleDownProtectsLoadedPods(t *testing.T, ctx context.Context, k8sClient client.Client, eppClient *webhook.EPPClient) {
	// Verify that the cost function protects loaded pods
	tests := []struct {
		name            string
		queue           int
		running         int
		shouldBeDeleted bool
	}{
		{"idle", 0, 0, true},
		{"lightly loaded", 1, 0, false},
		{"moderately loaded", 10, 2, false},
		{"heavily loaded", 50, 5, false},
	}

	baseCost := webhook.CalculateDeletionCost(0, 0)
	for _, tt := range tests {
		cost := webhook.CalculateDeletionCost(tt.queue, tt.running)
		shouldDelete := cost == baseCost

		if shouldDelete != tt.shouldBeDeleted {
			t.Errorf("Pod(%d queue, %d running) delete=%v, want %v",
				tt.queue, tt.running, shouldDelete, tt.shouldBeDeleted)
		}
	}

	t.Log("✓ Loaded pods are protected with higher deletion costs")
}

// testWebhookAdmissionFlow tests the complete webhook admission request flow.
func testWebhookAdmissionFlow(t *testing.T, ctx context.Context, k8sClient client.Client, eppClient *webhook.EPPClient) {
	// Create admission handler
	handler := webhook.NewAdmissionHandler(k8sClient, eppClient, "test-integration")

	// Create an eviction request
	evictionReview := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID("test-eviction-uid"),
			Kind: metav1.GroupKind{
				Group: "",
				Kind:  "Pod",
			},
			Resource: metav1.GroupVersionResource{
				Resource: "pods",
			},
			Subresource: "eviction",
			Name:        "inference-pod-0",
			Namespace:   "test-integration",
		},
	}

	// Marshal to JSON
	bodyBytes, err := json.Marshal(evictionReview)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	// Create HTTP request
	req := httptest.NewRequest("POST", "/mutate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.RequestURI = "" // httptest requires this

	// Create response recorder
	w := httptest.NewRecorder()

	// Handle the request
	handler.Handle(w, req)

	// Verify response
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Parse response
	var respReview admissionv1.AdmissionReview
	if err := json.NewDecoder(w.Body).Decode(&respReview); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !respReview.Response.Allowed {
		t.Errorf("Eviction should be admitted, got: %s", respReview.Response.Result.Message)
	}

	t.Logf("✓ Webhook admitted eviction request with message: %s", respReview.Response.Result.Message)
}

// testEPPFailureFallback tests that webhook fails open when EPP is unavailable.
func testEPPFailureFallback(t *testing.T, ctx context.Context, k8sClient client.Client) {
	// Create EPP client pointing to invalid endpoint
	eppClient := webhook.NewEPPClient("http://localhost:1")

	// Create admission handler
	handler := webhook.NewAdmissionHandler(k8sClient, eppClient, "test-integration")

	// Create an eviction request
	evictionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:         types.UID("test-uid"),
			Subresource: "eviction",
		},
	}

	bodyBytes, _ := json.Marshal(evictionReview)
	req := httptest.NewRequest("POST", "/mutate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.RequestURI = ""

	w := httptest.NewRecorder()
	handler.Handle(w, req)

	// Verify that even with EPP failure, we fail open (admit the request)
	var respReview admissionv1.AdmissionReview
	json.NewDecoder(w.Body).Decode(&respReview)

	if !respReview.Response.Allowed {
		t.Errorf("Webhook should fail open on EPP error, but denied: %s", respReview.Response.Result.Message)
	}

	t.Log("✓ Webhook fails open when EPP is unavailable")
}
