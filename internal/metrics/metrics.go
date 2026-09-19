// Package metrics records sync telemetry to both Prometheus and OpenTelemetry.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

const (
	namespace = "immich_skylight"
	meterName = "github.com/jfroy/immich-skylight"
)

// Recorder is the single entry point for emitting metrics.
type Recorder struct {
	syncRunsProm     *prometheus.CounterVec
	syncDurationProm prometheus.Histogram
	lastSuccessProm  prometheus.Gauge
	selectedProm     prometheus.Gauge
	sentTotalProm    prometheus.Gauge
	uploadsProm      *prometheus.CounterVec
	uploadBytesProm  prometheus.Histogram
	removalsProm     *prometheus.CounterVec
	authProm         *prometheus.CounterVec
	apiReqProm       *prometheus.CounterVec
	apiLatencyProm   *prometheus.HistogramVec

	syncRunsOTel     otelmetric.Int64Counter
	syncDurationOTel otelmetric.Float64Histogram
	uploadsOTel      otelmetric.Int64Counter
	uploadBytesOTel  otelmetric.Int64Histogram
	removalsOTel     otelmetric.Int64Counter
	authOTel         otelmetric.Int64Counter
	apiReqOTel       otelmetric.Int64Counter
	apiLatencyOTel   otelmetric.Float64Histogram
}

// New registers all collectors on registry and creates OTel instruments.
func New(registry *prometheus.Registry) (*Recorder, error) {
	r := &Recorder{
		syncRunsProm: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "sync", Name: "runs_total",
			Help: "Sync passes by result.",
		}, []string{"result"}),
		syncDurationProm: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "sync", Name: "duration_seconds",
			Help:    "Wall time of a sync pass.",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		}),
		lastSuccessProm: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "sync", Name: "last_success_timestamp_seconds",
			Help: "Unix time of the last successful sync pass.",
		}),
		selectedProm: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "selected_assets",
			Help: "Assets currently selected in Immich (favorites/tags).",
		}),
		sentTotalProm: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "tracked_assets",
			Help: "Assets recorded in state as present on the frame.",
		}),
		uploadsProm: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "uploads_total",
			Help: "Asset uploads by result and rendition.",
		}, []string{"result", "rendition"}),
		uploadBytesProm: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "upload_size_bytes",
			Help:    "Uploaded payload size.",
			Buckets: prometheus.ExponentialBuckets(100*1024, 2, 10),
		}),
		removalsProm: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "removals_total",
			Help: "Photos removed from the frame by result.",
		}, []string{"result"}),
		authProm: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "skylight", Name: "auth_total",
			Help: "Skylight authentication events by kind and result.",
		}, []string{"kind", "result"}),
		apiReqProm: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "client_requests_total",
			Help: "Outbound HTTP requests by target and status class.",
		}, []string{"target", "status"}),
		apiLatencyProm: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "http", Name: "client_request_duration_seconds",
			Help:    "Outbound HTTP request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"target", "status"}),
	}

	for _, c := range []prometheus.Collector{
		r.syncRunsProm, r.syncDurationProm, r.lastSuccessProm, r.selectedProm, r.sentTotalProm,
		r.uploadsProm, r.uploadBytesProm, r.removalsProm, r.authProm, r.apiReqProm, r.apiLatencyProm,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(c); err != nil {
			return nil, err
		}
	}

	m := otel.Meter(meterName)
	var err error
	if r.syncRunsOTel, err = m.Int64Counter("immich_skylight.sync.runs"); err != nil {
		return nil, err
	}
	if r.syncDurationOTel, err = m.Float64Histogram("immich_skylight.sync.duration", otelmetric.WithUnit("s")); err != nil {
		return nil, err
	}
	if r.uploadsOTel, err = m.Int64Counter("immich_skylight.uploads"); err != nil {
		return nil, err
	}
	if r.uploadBytesOTel, err = m.Int64Histogram("immich_skylight.upload.size", otelmetric.WithUnit("By")); err != nil {
		return nil, err
	}
	if r.removalsOTel, err = m.Int64Counter("immich_skylight.removals"); err != nil {
		return nil, err
	}
	if r.authOTel, err = m.Int64Counter("immich_skylight.skylight.auth"); err != nil {
		return nil, err
	}
	if r.apiReqOTel, err = m.Int64Counter("immich_skylight.http.client.requests"); err != nil {
		return nil, err
	}
	if r.apiLatencyOTel, err = m.Float64Histogram("immich_skylight.http.client.request.duration", otelmetric.WithUnit("s")); err != nil {
		return nil, err
	}
	return r, nil
}

// SyncFinished records the outcome of a pass.
func (r *Recorder) SyncFinished(ctx context.Context, result string, d time.Duration, selected, tracked int) {
	r.syncRunsProm.WithLabelValues(result).Inc()
	r.syncDurationProm.Observe(d.Seconds())
	r.selectedProm.Set(float64(selected))
	r.sentTotalProm.Set(float64(tracked))
	if result == "success" {
		r.lastSuccessProm.SetToCurrentTime()
	}
	attrs := otelmetric.WithAttributes(attribute.String("result", result))
	r.syncRunsOTel.Add(ctx, 1, attrs)
	r.syncDurationOTel.Record(ctx, d.Seconds(), attrs)
}

// Upload records one upload attempt.
func (r *Recorder) Upload(ctx context.Context, result, rendition string, bytes int) {
	r.uploadsProm.WithLabelValues(result, rendition).Inc()
	if result == "success" {
		r.uploadBytesProm.Observe(float64(bytes))
		r.uploadBytesOTel.Record(ctx, int64(bytes))
	}
	r.uploadsOTel.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("result", result), attribute.String("rendition", rendition)))
}

// Removed records photos deleted from the frame.
func (r *Recorder) Removed(ctx context.Context, result string, n int) {
	r.removalsProm.WithLabelValues(result).Add(float64(n))
	r.removalsOTel.Add(ctx, int64(n), otelmetric.WithAttributes(attribute.String("result", result)))
}

// Auth records a Skylight login/refresh.
func (r *Recorder) Auth(ctx context.Context, kind, result string) {
	r.authProm.WithLabelValues(kind, result).Inc()
	r.authOTel.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("kind", kind), attribute.String("result", result)))
}

// HTTPRequest records an outbound request (target = immich|skylight|storage).
func (r *Recorder) HTTPRequest(ctx context.Context, target, status string, d time.Duration) {
	r.apiReqProm.WithLabelValues(target, status).Inc()
	r.apiLatencyProm.WithLabelValues(target, status).Observe(d.Seconds())
	attrs := otelmetric.WithAttributes(attribute.String("target", target), attribute.String("status", status))
	r.apiReqOTel.Add(ctx, 1, attrs)
	r.apiLatencyOTel.Record(ctx, d.Seconds(), attrs)
}
