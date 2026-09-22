package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestWebhook(t *testing.T) {
	calls := 0
	srv := New(Options{Addr: ":0", Registry: prometheus.NewRegistry(), WebhookSecret: "s3cret", OnWebhook: func() { calls++ }})
	body := `{"type":"AssetV1","trigger":"AssetTagged","data":{"asset":{"id":"abc"}}}`

	do := func(method, secret string) int {
		r := httptest.NewRequest(method, "/webhook", strings.NewReader(body))
		if secret != "" {
			r.Header.Set(WebhookHeader, secret)
		}
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, r)
		return w.Code
	}
	if c := do(http.MethodPost, "wrong"); c != http.StatusForbidden {
		t.Errorf("wrong secret: %d", c)
	}
	if c := do(http.MethodPost, ""); c != http.StatusForbidden {
		t.Errorf("no secret: %d", c)
	}
	if c := do(http.MethodGet, "s3cret"); c != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", c)
	}
	if c := do(http.MethodPost, "s3cret"); c != http.StatusAccepted {
		t.Errorf("ok: %d", c)
	}
	if calls != 1 {
		t.Errorf("OnWebhook calls = %d", calls)
	}

	// Disabled when no secret is configured.
	off := New(Options{Addr: ":0", Registry: prometheus.NewRegistry()})
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	w := httptest.NewRecorder()
	off.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("disabled webhook: %d", w.Code)
	}
}
