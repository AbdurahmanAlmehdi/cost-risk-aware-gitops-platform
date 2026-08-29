// Package promapi is a minimal Prometheus query client.
//
// Hand-rolled rather than pulled from the official client library: the exporter issues
// exactly one kind of request (an instant query returning a vector), and a purpose-built
// client keeps the failure modes explicit and the dependency surface small.
package promapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// Sample is one labelled value from an instant query.
type Sample struct {
	Labels map[string]string
	Value  float64
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// Query runs an instant query and returns the resulting vector.
func (c *Client) Query(ctx context.Context, query string) ([]Sample, error) {
	endpoint := c.baseURL + "/api/v1/query?" + url.Values{"query": {query}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build query request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query prometheus: %w", err)
	}
	defer resp.Body.Close()

	var parsed queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode prometheus response (HTTP %d): %w", resp.StatusCode, err)
	}
	// Prometheus reports query errors in the body with a non-2xx status; surfacing its
	// message is far more useful than the status code alone when a PromQL expression is
	// malformed.
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus returned %s: %s (%s)", parsed.Status, parsed.Error, parsed.ErrorType)
	}
	if parsed.Data.ResultType != "vector" {
		return nil, fmt.Errorf("expected a vector result, got %q", parsed.Data.ResultType)
	}

	samples := make([]Sample, 0, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		// A sample is [unixTime, "value"], the value is a *string* so that Prometheus
		// can express NaN and Inf, which a JSON number cannot carry.
		var raw string
		if err := json.Unmarshal(r.Value[1], &raw); err != nil {
			return nil, fmt.Errorf("decode sample value: %w", err)
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("parse sample value %q: %w", raw, err)
		}
		samples = append(samples, Sample{Labels: r.Metric, Value: v})
	}
	return samples, nil
}
