package telemetry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bep/logg"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const scope = "github.com/gohugoio/hugo"

type contextKey struct{}

type instruments struct {
	tracer           trace.Tracer
	logger           log.Logger
	builds           metric.Int64Counter
	buildDuration    metric.Float64Histogram
	pages            metric.Int64Counter
	pageDuration     metric.Float64Histogram
	templateDuration metric.Float64Histogram
	cacheDuration    metric.Float64Histogram
	cacheRequests    metric.Int64Counter
	cachePotential   metric.Float64Gauge
	contentDuration  metric.Float64Histogram
}

func withProviders(ctx context.Context, version string, tp trace.TracerProvider, mp metric.MeterProvider, lp log.LoggerProvider) (context.Context, error) {
	m := mp.Meter(scope, metric.WithInstrumentationVersion(version), metric.WithSchemaURL(semconv.SchemaURL))
	i := &instruments{
		tracer: tp.Tracer(scope, trace.WithInstrumentationVersion(version), trace.WithSchemaURL(semconv.SchemaURL)),
		logger: lp.Logger(scope, log.WithInstrumentationVersion(version), log.WithSchemaURL(semconv.SchemaURL)),
	}
	var err error
	if i.builds, err = m.Int64Counter("hugo.build.count", metric.WithUnit("{build}"), metric.WithDescription("Number of site builds, including failed builds.")); err != nil {
		return ctx, err
	}
	if i.buildDuration, err = m.Float64Histogram("hugo.build.duration", metric.WithUnit("s"), metric.WithDescription("Duration of a site build."), metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 120, 300, 600)); err != nil {
		return ctx, err
	}
	if i.pages, err = m.Int64Counter("hugo.page.render.count", metric.WithUnit("{render}"), metric.WithDescription("Number of page output renders, including failed renders.")); err != nil {
		return ctx, err
	}
	if i.pageDuration, err = m.Float64Histogram("hugo.page.render.duration", metric.WithUnit("s"), metric.WithDescription("Duration of rendering and publishing a page output."), metric.WithExplicitBucketBoundaries(0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5)); err != nil {
		return ctx, err
	}
	if i.templateDuration, err = m.Float64Histogram("hugo.template.execution.duration", metric.WithUnit("s"), metric.WithDescription("Inclusive template execution duration, including nested template work."), metric.WithExplicitBucketBoundaries(0.000001, 0.000005, 0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5)); err != nil {
		return ctx, err
	}
	if i.cacheDuration, err = m.Float64Histogram("hugo.partial.cache.lookup.duration", metric.WithUnit("s"), metric.WithDescription("Cached partial lookup duration, including computation on misses and waiting on hits."), metric.WithExplicitBucketBoundaries(0.000001, 0.000005, 0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5)); err != nil {
		return ctx, err
	}
	if i.cacheRequests, err = m.Int64Counter("hugo.partial.cache.request.count", metric.WithUnit("{request}"), metric.WithDescription("Cached partial requests by hit or miss, including failed requests.")); err != nil {
		return ctx, err
	}
	if i.cachePotential, err = m.Float64Gauge("hugo.template.cache.potential", metric.WithUnit("1"), metric.WithDescription("Hugo output similarity hint as a ratio from zero to one; not a guarantee of safe caching.")); err != nil {
		return ctx, err
	}
	if i.contentDuration, err = m.Float64Histogram("hugo.page.content.render.duration", metric.WithUnit("s"), metric.WithDescription("Duration of rendering page content, attributed to its first use in any template."), metric.WithExplicitBucketBoundaries(0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5)); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, contextKey{}, i), nil
}

func TemplateMetricsEnabled(ctx context.Context) bool {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	return ok && i.templateDuration.Enabled(ctx)
}

func TemplateFinished(ctx context.Context, name string, start time.Time) {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok {
		return
	}
	i.templateDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("hugo.template.name", name)))
}

func ContentRendered(ctx context.Context, start time.Time, attrs ...attribute.KeyValue) {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok {
		return
	}
	i.contentDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

func PartialCacheMetricsEnabled(ctx context.Context) bool {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	return ok && (i.cacheDuration.Enabled(ctx) || i.cacheRequests.Enabled(ctx))
}

func PartialCacheFinished(ctx context.Context, name string, elapsed time.Duration, hit bool) {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok {
		return
	}
	result := "miss"
	if hit {
		result = "hit"
	}
	opts := metric.WithAttributes(attribute.String("hugo.template.name", name), attribute.String("hugo.partial.cache.result", result))
	i.cacheDuration.Record(ctx, elapsed.Seconds(), opts)
	i.cacheRequests.Add(ctx, 1, opts)
}

func CachePotential(ctx context.Context, scores map[string]int) {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok || !i.cachePotential.Enabled(ctx) {
		return
	}
	for name, score := range scores {
		i.cachePotential.Record(ctx, float64(score)/100, metric.WithAttributes(attribute.String("hugo.template.name", name)))
	}
}

func Enabled(ctx context.Context) bool {
	return ctx != nil && (ctx.Value(contextKey{}) != nil || trace.SpanFromContext(ctx).IsRecording())
}

func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return StartWithOptions(ctx, name, trace.WithAttributes(attrs...))
}

func StartWithOptions(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	if i, ok := ctx.Value(contextKey{}).(*instruments); ok {
		return i.tracer.Start(ctx, name, opts...)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(scope).Start(ctx, name, opts...)
}

func End(span trace.Span, err error, opts ...trace.SpanEndOption) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(semconv.ErrorType(err))
	}
	span.End(opts...)
}

// StartCommand starts a CLI span following the cli semantic conventions.
func StartCommand(ctx context.Context, name string) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{semconv.ProcessPID(os.Getpid())}
	if exe, err := os.Executable(); err == nil {
		attrs = append(attrs, semconv.ProcessExecutableName(filepath.Base(exe)), semconv.ProcessExecutablePath(exe))
	}
	return Start(ctx, name, attrs...)
}

// EndCommand ends a span started with StartCommand, see main for the exit code.
func EndCommand(span trace.Span, err error) {
	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	span.SetAttributes(semconv.ProcessExitCode(exitCode))
	End(span, err)
}

func BuildFinished(ctx context.Context, start time.Time, rebuild bool, err error) {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok {
		return
	}
	attrs := []attribute.KeyValue{attribute.Bool("hugo.build.rebuild", rebuild)}
	if err != nil {
		attrs = append(attrs, semconv.ErrorType(err))
	}
	opts := metric.WithAttributes(attrs...)
	i.builds.Add(ctx, 1, opts)
	i.buildDuration.Record(ctx, time.Since(start).Seconds(), opts)

	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue("Build completed"))
	if err != nil {
		r.SetSeverity(log.SeverityError)
		r.SetSeverityText("ERROR")
		r.SetBody(attribute.StringValue("Build failed"))
		r.SetErr(err)
	}
	r.AddAttributes(attrs...)
	i.logger.Emit(ctx, r)
}

func PageFinished(ctx context.Context, start time.Time, err error, attrs ...attribute.KeyValue) {
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok {
		return
	}
	if err != nil {
		attrs = append(attrs, semconv.ErrorType(err))
	}
	opts := metric.WithAttributes(attrs...)
	i.pages.Add(ctx, 1, opts)
	i.pageDuration.Record(ctx, time.Since(start).Seconds(), opts)
}

func ForwardLog(ctx context.Context, e *logg.Entry) {
	if ctx == nil {
		return
	}
	i, ok := ctx.Value(contextKey{}).(*instruments)
	if !ok {
		return
	}
	severity := log.SeverityInfo
	switch e.Level {
	case logg.LevelTrace:
		severity = log.SeverityTrace
	case logg.LevelDebug:
		severity = log.SeverityDebug
	case logg.LevelWarn:
		severity = log.SeverityWarn
	case logg.LevelError:
		severity = log.SeverityError
	}
	if !i.logger.Enabled(ctx, log.EnabledParameters{Severity: severity}) {
		return
	}
	var r log.Record
	r.SetTimestamp(e.Timestamp)
	r.SetSeverity(severity)
	r.SetSeverityText(e.Level.String())
	r.SetBody(attribute.StringValue(e.Message))
	for _, f := range e.Fields {
		r.AddAttributes(attribute.String("hugo.log."+f.Name, fmt.Sprint(f.Value)))
	}
	i.logger.Emit(ctx, r)
}
