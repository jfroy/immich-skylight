// Package observability wires OpenTelemetry (traces, metrics, logs) and slog.
//
// OTLP export is configured entirely through the standard OTEL_* environment
// variables (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, OTEL_EXPORTER_OTLP_LOGS_ENDPOINT,
// OTEL_RESOURCE_ATTRIBUTES, OTEL_SDK_DISABLED, ...). When no endpoint is set,
// no exporters are started; Prometheus /metrics is served independently.
package observability

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Options configures Init.
type Options struct {
	ServiceName    string
	ServiceVersion string
	// Stdout is the destination for JSON logs (defaults to os.Stdout).
	Stdout *os.File
	// Level is the minimum log level.
	Level slog.Level
}

// Providers holds everything that must be shut down at exit.
type Providers struct {
	// Logger is the process logger: JSON to stdout, correlated with the active
	// trace, and (when OTLP is configured) mirrored to the OTLP log exporter.
	Logger *slog.Logger

	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider
	lp *sdklog.LoggerProvider
}

// Init sets up global propagators, tracer/meter/logger providers and returns
// the process logger.
func Init(ctx context.Context, o Options) (*Providers, error) {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(o.ServiceName),
			semconv.ServiceVersion(o.ServiceVersion),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}

	p := &Providers{}
	stdout := &traceHandler{Handler: slog.NewJSONHandler(o.Stdout, &slog.HandlerOptions{Level: o.Level, ReplaceAttr: humanizeAttr})}
	handlers := []slog.Handler{stdout}

	// OTel metrics are exported over OTLP only. Prometheus scraping is served by
	// the native client_golang collectors in internal/metrics, which mirror the
	// same measurements; bridging both into one registry would collide on names.
	metricReaders := []sdkmetric.Option{sdkmetric.WithResource(res)}

	enabled := otlpConfigured()
	if enabled {
		traceExp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		p.tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))

		metricExp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			_ = p.tp.Shutdown(ctx)
			return nil, err
		}
		metricReaders = append(metricReaders, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)))

		logExp, err := otlploghttp.New(ctx)
		if err != nil {
			_ = p.tp.Shutdown(ctx)
			return nil, err
		}
		p.lp = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)), sdklog.WithResource(res))
		handlers = append(handlers, otelslog.NewHandler(o.ServiceName, otelslog.WithLoggerProvider(p.lp)))
	} else {
		// A tracer provider without exporters still yields valid span contexts
		// so log correlation works, but nothing is exported.
		p.tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.NeverSample()))
	}
	otel.SetTracerProvider(p.tp)

	p.mp = sdkmetric.NewMeterProvider(metricReaders...)
	otel.SetMeterProvider(p.mp)

	var h slog.Handler = stdout
	if len(handlers) > 1 {
		h = slog.NewMultiHandler(handlers...)
	}
	p.Logger = slog.New(h)
	slog.SetDefault(p.Logger)
	p.Logger.Info("observability initialized", "otlp", enabled, "service", o.ServiceName, "version", o.ServiceVersion)
	return p, nil
}

// Shutdown flushes and stops all providers.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.tp != nil {
		errs = append(errs, p.tp.Shutdown(ctx))
	}
	if p.mp != nil {
		errs = append(errs, p.mp.Shutdown(ctx))
	}
	if p.lp != nil {
		errs = append(errs, p.lp.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func otlpConfigured() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// traceHandler injects trace_id/span_id into records emitted within a span.
type traceHandler struct{ slog.Handler }

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}

// humanizeAttr renders durations as strings ("15m0s") instead of nanoseconds.
func humanizeAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindDuration {
		return slog.String(a.Key, a.Value.Duration().String())
	}
	return a
}

// ParseLevel converts a string to a slog.Level (defaults to Info).
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(s)))); err != nil {
		return slog.LevelInfo
	}
	return l
}
