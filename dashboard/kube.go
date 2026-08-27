package main

// A minimal Kubernetes and Prometheus client, spoken over plain HTTP with the pod's own
// ServiceAccount token.
//
// client-go would be the obvious choice and is deliberately avoided: this service needs
// four read operations and one patch, and pulling in the full client library to get them
// would add tens of megabytes and a dependency tree larger than the rest of the platform.
// The same reasoning produced the hand-rolled Prometheus client in M4.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

type kube struct {
	base  string
	token string
	http  *http.Client
}

func newKube() (*kube, error) {
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster CA is not valid PEM")
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running inside a cluster")
	}
	return &kube{
		base:  fmt.Sprintf("https://%s:%s", host, port),
		token: string(token),
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
	}, nil
}

func (k *kube) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kubernetes %s %s: %s", method, path, resp.Status)
	}
	return raw, nil
}

func (k *kube) get(ctx context.Context, path string, out any) error {
	raw, err := k.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// prom queries Prometheus for a single scalar. Errors are returned rather than defaulted
// to zero: a dashboard tile reading "0" when the truth is "unknown" is the same failure
// M4's freshness metric exists to prevent.
func prom(ctx context.Context, base, query string) (float64, error) {
	endpoint := base + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, err
	}
	if parsed.Status != "success" || len(parsed.Data.Result) == 0 {
		return 0, fmt.Errorf("no data")
	}
	var s string
	if err := json.Unmarshal(parsed.Data.Result[0].Value[1], &s); err != nil {
		return 0, err
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, err
	}
	return f, nil
}
