package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PodMetrics holds per-pod queue and request metrics from EPP.
type PodMetrics struct {
	Pod                 string `json:"pod"`
	WaitingQueueSize    int    `json:"waiting_queue_size"`
	RunningRequestsSize int    `json:"running_requests_size"`
}

// EPPClient queries the Endpoint Picker for per-pod metrics.
type EPPClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewEPPClient creates a new EPP client.
func NewEPPClient(baseURL string) *EPPClient {
	return &EPPClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// GetEndpoints queries EPP for current per-pod metrics.
func (c *EPPClient) GetEndpoints(ctx context.Context) ([]PodMetrics, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/endpoints", c.baseURL), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("EPP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("EPP returned %d: %s", resp.StatusCode, string(body))
	}

	var endpoints []PodMetrics
	if err := json.NewDecoder(resp.Body).Decode(&endpoints); err != nil {
		return nil, fmt.Errorf("failed to parse EPP response: %w", err)
	}

	return endpoints, nil
}
