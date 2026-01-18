package telemetry

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
}

type ShutdownFunc func(context.Context) error

func Init(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = "ferretdb-controller"
	}
	baseHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		slog.SetDefault(slog.New(baseHandler))
		return func(context.Context) error { return nil }, nil
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		slog.SetDefault(slog.New(baseHandler))
		return func(context.Context) error { return nil }, err
	}

	options := getOtlpOptions()
	traceExp, err := otlptracegrpc.New(ctx, traceExporterOptions(options)...)
	if err != nil {
		slog.SetDefault(slog.New(baseHandler))
		return func(context.Context) error { return nil }, err
	}

	metricExp, err := otlpmetricgrpc.New(ctx, metricExporterOptions(options)...)
	if err != nil {
		slog.SetDefault(slog.New(baseHandler))
		return func(context.Context) error { return nil }, err
	}

	logExp, err := otlploggrpc.New(ctx, logExporterOptions(options)...)
	if err != nil {
		slog.SetDefault(slog.New(baseHandler))
		return func(context.Context) error { return nil }, err
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	otel.SetTracerProvider(tracerProvider)

	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(15*time.Second))),
	)
	otel.SetMeterProvider(meterProvider)

	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	global.SetLoggerProvider(loggerProvider)

	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	otelHandler := otelslog.NewHandler(cfg.ServiceName, otelslog.WithLoggerProvider(loggerProvider))
	slog.SetDefault(slog.New(multiHandler{handlers: []slog.Handler{baseHandler, otelHandler}}))

	return func(ctx context.Context) error {
		var shutdownErr error
		if err := loggerProvider.Shutdown(ctx); err != nil {
			shutdownErr = err
		}
		if err := meterProvider.Shutdown(ctx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
		if err := tracerProvider.Shutdown(ctx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
		return shutdownErr
	}, nil
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{}
	if strings.TrimSpace(cfg.ServiceName) != "" {
		attrs = append(attrs, attribute.String("service.name", cfg.ServiceName))
	}
	if strings.TrimSpace(cfg.ServiceVersion) != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	if strings.TrimSpace(cfg.Environment) != "" {
		attrs = append(attrs, attribute.String("deployment.environment", cfg.Environment))
	}
	return resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(attrs...),
	)
}

type otlpOptions struct {
	endpoint string
	insecure bool
}

func getOtlpOptions() otlpOptions {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	insecure := strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")), "true")
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err == nil && parsed.Host != "" {
			endpoint = parsed.Host
			if parsed.Scheme == "http" {
				insecure = true
			}
		}
	}
	return otlpOptions{
		endpoint: endpoint,
		insecure: insecure,
	}
}

func traceExporterOptions(options otlpOptions) []otlptracegrpc.Option {
	opts := []otlptracegrpc.Option{}
	if options.endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(options.endpoint))
	}
	if options.insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return opts
}

func metricExporterOptions(options otlpOptions) []otlpmetricgrpc.Option {
	opts := []otlpmetricgrpc.Option{}
	if options.endpoint != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(options.endpoint))
	}
	if options.insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return opts
}

func logExporterOptions(options otlpOptions) []otlploggrpc.Option {
	opts := []otlploggrpc.Option{}
	if options.endpoint != "" {
		opts = append(opts, otlploggrpc.WithEndpoint(options.endpoint))
	}
	if options.insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	return opts
}

type multiHandler struct {
	handlers []slog.Handler
}

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range m.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, record slog.Record) error {
	var handleErr error
	for _, handler := range m.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record); err != nil && handleErr == nil {
			handleErr = err
		}
	}
	return handleErr
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, 0, len(m.handlers))
	for _, handler := range m.handlers {
		handlers = append(handlers, handler.WithAttrs(attrs))
	}
	return multiHandler{handlers: handlers}
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, 0, len(m.handlers))
	for _, handler := range m.handlers {
		handlers = append(handlers, handler.WithGroup(name))
	}
	return multiHandler{handlers: handlers}
}
