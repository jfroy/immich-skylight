// Package httpx provides an instrumented http.RoundTripper shared by all
// outbound clients: OpenTelemetry spans via otelhttp plus per-target request
// counters/latency.
package httpx

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Metrics is the subset of metrics.Recorder the transport needs.
type Metrics interface {
	HTTPRequest(ctx context.Context, target, status string, d time.Duration)
}

// Classifier names the logical target of a request for metric labels.
type Classifier func(*http.Request) string

// NewTransport returns a RoundTripper wrapping base (or a sane default) with
// tracing and metrics.
func NewTransport(base http.RoundTripper, m Metrics, classify Classifier) http.RoundTripper {
	if base == nil {
		base = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			MaxIdleConns:          20,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		}
	}
	if classify == nil {
		classify = HostClassifier
	}
	rt := &metricsTransport{base: base, m: m, classify: classify}
	return otelhttp.NewTransport(rt, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return r.Method + " " + classify(r)
	}))
}

// NewClient returns an http.Client using NewTransport.
func NewClient(m Metrics, classify Classifier, timeout time.Duration) *http.Client {
	return &http.Client{Transport: NewTransport(nil, m, classify), Timeout: timeout}
}

// HostClassifier maps well-known hosts to short target names.
func HostClassifier(r *http.Request) string {
	h := r.URL.Hostname()
	switch {
	case strings.HasSuffix(h, "ourskylight.com"):
		return "skylight"
	case strings.Contains(h, "amazonaws.com") || strings.Contains(h, "cloudfront.net"):
		return "storage"
	}
	return h
}

type metricsTransport struct {
	base     http.RoundTripper
	m        Metrics
	classify Classifier
}

func (t *metricsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(r)
	if t.m != nil {
		status := "error"
		if err == nil {
			status = strconv.Itoa(resp.StatusCode/100) + "xx"
		}
		t.m.HTTPRequest(r.Context(), t.classify(r), status, time.Since(start))
	}
	return resp, err
}
