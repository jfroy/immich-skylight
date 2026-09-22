// Package server exposes /metrics, /healthz and /readyz.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options configures New.
type Options struct {
	Addr     string
	Registry *prometheus.Registry
	// Ready is consulted by /readyz.
	Ready func() bool
	// WebhookSecret enables POST /webhook when non-empty. Requests must send it
	// in the X-Webhook-Secret header (the Immich workflow webhook action's
	// custom header). On success OnWebhook is invoked.
	WebhookSecret string
	OnWebhook     func()
	Logger        *slog.Logger
}

// New builds the HTTP server.
func New(o Options) *http.Server {
	reg, ready := o.Registry, o.Ready
	mux := http.NewServeMux()
	if o.WebhookSecret != "" {
		mux.HandleFunc("POST /webhook", webhookHandler(o))
	}
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil && !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return &http.Server{
		Addr:              o.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// WebhookHeader carries the shared secret.
const WebhookHeader = "X-Webhook-Secret"

// webhookPayload is the subset of the Immich workflow webhook body we look at.
// The payload is only a nudge: the sync pass re-reads Immich as the source of
// truth, so nothing here is trusted beyond logging.
type webhookPayload struct {
	Type    string `json:"type"`
	Trigger string `json:"trigger"`
	Data    struct {
		Asset struct {
			ID string `json:"id"`
		} `json:"asset"`
	} `json:"data"`
}

func webhookHandler(o Options) http.HandlerFunc {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(WebhookHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(o.WebhookSecret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var p webhookPayload
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(body, &p) // best effort; malformed bodies still trigger
		log.InfoContext(r.Context(), "webhook received", "type", p.Type, "trigger", p.Trigger, "asset", p.Data.Asset.ID)
		if o.OnWebhook != nil {
			o.OnWebhook()
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
