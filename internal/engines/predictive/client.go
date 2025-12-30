/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package predictive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// LatencyPredictorClient defines the interface for communicating with the Latency Predictor service/sidecar.
type LatencyPredictorClient interface {
	// PredictITL returns the predicted Inter-Token Latency (p90) in milliseconds.
	PredictITL(ctx context.Context, modelID string, features map[string]float64) (float64, error)
	// PredictTTFT returns the predicted Time To First Token (p90) in milliseconds.
	PredictTTFT(ctx context.Context, modelID string, features map[string]float64) (float64, error)
}

// MockLatencyPredictorClient is a mock implementation for testing and development.
type MockLatencyPredictorClient struct {
	MockITL  float64
	MockTTFT float64
}

func (m *MockLatencyPredictorClient) PredictITL(ctx context.Context, modelID string, features map[string]float64) (float64, error) {
	return m.MockITL, nil
}

func (m *MockLatencyPredictorClient) PredictTTFT(ctx context.Context, modelID string, features map[string]float64) (float64, error) {
	return m.MockTTFT, nil
}

// HTTPLatencyPredictorClient implements LatencyPredictorClient using HTTP/JSON.
type HTTPLatencyPredictorClient struct {
	BaseURL string
	Client  *http.Client
}

type PredictionRequest struct {
	KvCachePercentage  float64 `json:"kv_cache_percentage"`
	InputTokenLength   int     `json:"input_token_length"`
	NumRequestWaiting  int     `json:"num_request_waiting"`
	NumRequestRunning  int     `json:"num_request_running"`
	NumTokensGenerated int     `json:"num_tokens_generated"`
	PrefixCacheScore   float64 `json:"prefix_cache_score"`
}

type PredictionResponse struct {
	TtftMs float64 `json:"ttft_ms"`
	TpotMs float64 `json:"tpot_ms"`
}

func NewHTTPLatencyPredictorClient(baseURL string) *HTTPLatencyPredictorClient {
	return &HTTPLatencyPredictorClient{
		BaseURL: baseURL,
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *HTTPLatencyPredictorClient) predict(ctx context.Context, endpoint string, features map[string]float64) (*PredictionResponse, error) {
	reqBody := PredictionRequest{
		KvCachePercentage:  features["kv_cache_percentage"],
		InputTokenLength:   int(features["input_token_length"]),
		NumRequestWaiting:  int(features["num_request_waiting"]),
		NumRequestRunning:  int(features["num_request_running"]),
		NumTokensGenerated: int(features["num_tokens_generated"]),
		PrefixCacheScore:   features["prefix_cache_score"],
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s%s", c.BaseURL, endpoint)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var respBody PredictionResponse
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &respBody, nil
}

func (c *HTTPLatencyPredictorClient) PredictITL(ctx context.Context, modelID string, features map[string]float64) (float64, error) {
	// EPP server seems to ignore ModelID in URL for single prediction, or expects it implied by service?
	// But OpenAPI showed /predict.
	// We call /predict.
	resp, err := c.predict(ctx, "/predict", features)
	if err != nil {
		return 0, err
	}
	return resp.TpotMs, nil
}

func (c *HTTPLatencyPredictorClient) PredictTTFT(ctx context.Context, modelID string, features map[string]float64) (float64, error) {
	resp, err := c.predict(ctx, "/predict", features)
	if err != nil {
		return 0, err
	}
	return resp.TtftMs, nil
}
