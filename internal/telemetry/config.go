package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/propagators/envcar"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// FromEnv configures opt-in OTLP/HTTP telemetry and continues a trace passed
// in TRACEPARENT/TRACESTATE/BAGGAGE. The returned shutdown flushes all signals.
func FromEnv(ctx context.Context, version string) (context.Context, func() error, error) {
	var shutdowns []func(context.Context) error
	shutdown := sync.OnceValue(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var errs []error
		for _, f := range shutdowns {
			errs = append(errs, f(ctx))
		}
		return errors.Join(errs...)
	})
	if !strings.EqualFold(os.Getenv("HUGO_OTEL_ENABLED"), "true") || strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return ctx, shutdown, nil
	}
	for _, signal := range []string{"TRACES", "METRICS", "LOGS"} {
		if _, err := enabled(signal); err != nil {
			return ctx, shutdown, err
		}
	}
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(semconv.ServiceName("hugo"), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(), resource.WithFromEnv(),
	)
	if err != nil {
		return ctx, shutdown, err
	}
	var tp trace.TracerProvider = tracenoop.NewTracerProvider()
	var mp metric.MeterProvider = metricnoop.NewMeterProvider()
	var lp log.LoggerProvider = lognoop.NewLoggerProvider()
	if on, _ := enabled("TRACES"); on {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return ctx, shutdown, err
		}
		p := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
		tp = p
		shutdowns = append(shutdowns, p.Shutdown)
	}
	if on, _ := enabled("METRICS"); on {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return ctx, shutdown, err
		}
		p := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
		mp = p
		shutdowns = append(shutdowns, p.Shutdown)
	}
	if on, _ := enabled("LOGS"); on {
		exporter, err := otlploghttp.New(ctx)
		if err != nil {
			return ctx, shutdown, err
		}
		p := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
		lp = p
		shutdowns = append(shutdowns, p.Shutdown)
	}
	ctx = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}).Extract(ctx, &envcar.Carrier{})
	ctx, err = withProviders(ctx, version, tp, mp, lp)
	return ctx, shutdown, err
}

func enabled(signal string) (bool, error) {
	exporter := strings.ToLower(os.Getenv("OTEL_" + signal + "_EXPORTER"))
	if exporter == "none" {
		return false, nil
	}
	if exporter != "" && exporter != "otlp" {
		return false, fmt.Errorf("unsupported OTEL_%s_EXPORTER %q (use otlp or none)", signal, exporter)
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && !strings.EqualFold(protocol, "http/protobuf") {
		return false, fmt.Errorf("unsupported OTLP protocol %q (use http/protobuf)", protocol)
	}
	return true, nil
}
