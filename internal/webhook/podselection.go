package webhook

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	DeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
	QueueDepthAnnotation   = "inferno/queue-depth"
	RunningRequestsAnnotation = "inferno/running-requests"
)

// PodSelector calculates and applies deletion costs to pods.
type PodSelector struct {
	kubeClient client.Client
	eppClient  *EPPClient
	namespace  string
}

// NewPodSelector creates a new pod selector.
func NewPodSelector(kubeClient client.Client, eppClient *EPPClient, namespace string) *PodSelector {
	return &PodSelector{
		kubeClient: kubeClient,
		eppClient:  eppClient,
		namespace:  namespace,
	}
}

// CalculateDeletionCost returns the pod deletion priority based on queue depth and running requests.
// Higher cost = higher priority to keep (lower priority to delete)
// Lower cost = lower priority to keep (higher priority to delete)
// Formula: cost = (queue_depth × 10) + (running_requests × 5) - 100
func CalculateDeletionCost(queueDepth, runningRequests int) int {
	cost := (queueDepth * 10) + (runningRequests * 5) - 100

	if cost > 1000 {
		return 1000 // Cap to prevent overflow
	}

	return cost
}

// UpdatePodDeletionCosts queries EPP and updates pod deletion cost annotations.
func (ps *PodSelector) UpdatePodDeletionCosts(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Query EPP for current metrics
	endpoints, err := ps.eppClient.GetEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("failed to get endpoints: %w", err)
	}

	if len(endpoints) == 0 {
		logger.Info("No endpoints returned from EPP")
		return nil
	}

	// Build a map of pod name → metrics
	metricsMap := make(map[string]PodMetrics)
	for _, ep := range endpoints {
		metricsMap[ep.Pod] = ep
	}

	// List pods in namespace
	var pods corev1.PodList
	if err := ps.kubeClient.List(ctx, &pods, client.InNamespace(ps.namespace)); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	updatedCount := 0
	for i := range pods.Items {
		pod := &pods.Items[i]

		// Skip pods not in EPP
		metrics, exists := metricsMap[pod.Name]
		if !exists {
			logger.V(1).Info("Pod not found in EPP", "pod", pod.Name)
			continue
		}

		// Calculate deletion cost
		cost := CalculateDeletionCost(metrics.WaitingQueueSize, metrics.RunningRequestsSize)

		// Check if annotation needs updating
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}

		currentCostStr := pod.Annotations[DeletionCostAnnotation]
		newCostStr := strconv.Itoa(cost)

		if currentCostStr == newCostStr {
			continue // No change needed
		}

		// Update annotations
		pod.Annotations[DeletionCostAnnotation] = newCostStr
		pod.Annotations[QueueDepthAnnotation] = strconv.Itoa(metrics.WaitingQueueSize)
		pod.Annotations[RunningRequestsAnnotation] = strconv.Itoa(metrics.RunningRequestsSize)

		if err := ps.kubeClient.Update(ctx, pod); err != nil {
			logger.Error(err, "failed to update pod annotations", "pod", pod.Name)
			continue
		}

		logger.Info("Updated pod deletion cost",
			"pod", pod.Name,
			"cost", cost,
			"queue", metrics.WaitingQueueSize,
			"running", metrics.RunningRequestsSize,
		)
		updatedCount++
	}

	logger.Info("Updated pod deletion costs", "updated", updatedCount, "total", len(pods.Items))
	return nil
}

// PatchPodDeletionCost patches a single pod with a deletion cost annotation.
func (ps *PodSelector) PatchPodDeletionCost(ctx context.Context, pod *corev1.Pod, cost int) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[DeletionCostAnnotation] = strconv.Itoa(cost)

	if err := ps.kubeClient.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("failed to patch pod: %w", err)
	}

	return nil
}
