package hugolib

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/gohugoio/hugo/internal/telemetry"
	"github.com/gohugoio/hugo/resources/resource_transformers/tocss/dartsass"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestBuildTelemetryExports(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			c := qt.New(t)
			var mu sync.Mutex
			payloads := make(map[string][][]byte)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				payloads[r.URL.Path] = append(payloads[r.URL.Path], b)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			t.Cleanup(server.Close)

			t.Setenv("HUGO_OTEL_ENABLED", "true")
			t.Setenv("OTEL_SDK_DISABLED", "false")
			t.Setenv("OTEL_SERVICE_NAME", "hugo-test")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.name=build")
			t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
			t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "60000")
			t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "60000")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "60000")
			for signal, path := range map[string]string{"TRACES": "traces", "METRICS": "metrics", "LOGS": "logs"} {
				t.Setenv("OTEL_"+signal+"_EXPORTER", "otlp")
				t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"_PROTOCOL", "http/protobuf")
				t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT", server.URL+"/v1/"+path)
				t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"_COMPRESSION", "none")
			}
			ctx, shutdown, err := telemetry.FromEnv(context.Background(), "test-version")
			c.Assert(err, qt.IsNil)
			t.Cleanup(func() { c.Check(shutdown(), qt.IsNil) })
			files := `
-- hugo.toml --
disableKinds = ["taxonomy", "term", "RSS", "sitemap"]
templateMetrics = true
templateMetricsHints = true
-- content/p1.md --
---
title: P1
---
Hello telemetry.
-- layouts/page.html --
{{ .Content }}
-- layouts/list.html --
{{ partialCached "c.html" . }}{{ partialCached "c.html" . }}
-- layouts/_partials/c.html --
{{ .Title }}
`
			if fail {
				files += "\n-- layouts/page.html --\n{{ errorf `render failed` }}\n"
			}
			b, buildErr := TestE(t, files, func(cfg *IntegrationTestConfig) { cfg.BuildCfg.Context = ctx })
			if fail {
				c.Assert(buildErr, qt.ErrorMatches, "logged 1 error\\(s\\)")
				b.AssertLogContains("render failed")
			} else {
				c.Assert(buildErr, qt.IsNil)
				b.AssertFileContent("public/p1/index.html", "Hello telemetry.")
			}
			c.Assert(shutdown(), qt.IsNil)
			mu.Lock()
			defer mu.Unlock()
			c.Assert(payloads, qt.HasLen, 3)

			var spans []*tracepb.Span
			for _, b := range payloads["/v1/traces"] {
				var req collectortrace.ExportTraceServiceRequest
				c.Assert(proto.Unmarshal(b, &req), qt.IsNil)
				for _, rs := range req.ResourceSpans {
					attrs := make(map[string]string)
					for _, a := range rs.Resource.Attributes {
						attrs[a.Key] = a.Value.GetStringValue()
					}
					c.Assert(attrs["service.name"], qt.Equals, "hugo-test")
					c.Assert(attrs["service.version"], qt.Equals, "test-version")
					for _, ss := range rs.ScopeSpans {
						spans = append(spans, ss.Spans...)
					}
				}
			}
			byName := make(map[string]*tracepb.Span)
			for _, span := range spans {
				byName[span.Name] = span
			}
			root := byName["hugo.build"]
			c.Assert(root, qt.IsNotNil)
			c.Assert(byName["hugo.build.process"], qt.IsNotNil)
			c.Assert(byName["hugo.build.assemble"], qt.IsNotNil)
			render := byName["hugo.build.render"]
			c.Assert(render, qt.IsNotNil)
			c.Assert(render.ParentSpanId, qt.DeepEquals, root.SpanId)
			for _, span := range spans {
				c.Assert(span.TraceId, qt.DeepEquals, root.TraceId)
				if span.Name == "hugo.page.render" {
					c.Assert(span.ParentSpanId, qt.DeepEquals, render.SpanId)
				}
			}
			c.Assert(byName["hugo.page.render"], qt.IsNotNil)
			if fail {
				c.Assert(root.Status.Code, qt.Equals, tracepb.Status_STATUS_CODE_ERROR)
			} else {
				c.Assert(root.Status.GetCode(), qt.Equals, tracepb.Status_STATUS_CODE_UNSET)
			}

			metrics := make(map[string]bool)
			for _, b := range payloads["/v1/metrics"] {
				var req collectormetric.ExportMetricsServiceRequest
				c.Assert(proto.Unmarshal(b, &req), qt.IsNil)
				for _, rm := range req.ResourceMetrics {
					for _, sm := range rm.ScopeMetrics {
						for _, m := range sm.Metrics {
							metrics[m.Name] = true
							switch m.Name {
							case "hugo.build.count":
								c.Assert(m.GetSum().DataPoints, qt.HasLen, 1)
								c.Assert(m.GetSum().DataPoints[0].GetAsInt(), qt.Equals, int64(1))
							case "hugo.template.execution.duration":
								counts := make(map[string]uint64)
								for _, dp := range m.GetHistogram().DataPoints {
									counts[dp.Attributes[0].Value.GetStringValue()] = dp.Count
								}
								c.Assert(counts["list.html"], qt.Equals, uint64(1))
								c.Assert(counts["_partials/c.html"], qt.Equals, uint64(1))
							case "hugo.partial.cache.request.count":
								results := make(map[string]int64)
								for _, dp := range m.GetSum().DataPoints {
									for _, a := range dp.Attributes {
										if a.Key == "hugo.partial.cache.result" {
											results[a.Value.GetStringValue()] = dp.GetAsInt()
										}
									}
								}
								c.Assert(results, qt.DeepEquals, map[string]int64{"hit": 1, "miss": 1})
							case "hugo.page.content.render.duration":
								c.Assert(m.GetHistogram().DataPoints, qt.HasLen, 1)
								c.Assert(m.GetHistogram().DataPoints[0].Count, qt.Equals, uint64(1))
							case "hugo.template.cache.potential":
								c.Assert(m.GetGauge().DataPoints, qt.Not(qt.HasLen), 0)
							}
						}
					}
				}
			}
			want := map[string]bool{"hugo.build.count": true, "hugo.build.duration": true, "hugo.page.render.count": true, "hugo.page.render.duration": true, "hugo.template.execution.duration": true, "hugo.partial.cache.lookup.duration": true, "hugo.partial.cache.request.count": true, "hugo.template.cache.potential": true}
			if !fail {
				want["hugo.page.content.render.duration"] = true
			}
			c.Assert(metrics, qt.DeepEquals, want)

			logCount := 0
			for _, b := range payloads["/v1/logs"] {
				var req collectorlog.ExportLogsServiceRequest
				c.Assert(proto.Unmarshal(b, &req), qt.IsNil)
				for _, rl := range req.ResourceLogs {
					for _, sl := range rl.ScopeLogs {
						for _, record := range sl.LogRecords {
							logCount++
							c.Assert(record.TraceId, qt.DeepEquals, root.TraceId)
							c.Assert(record.SpanId, qt.DeepEquals, root.SpanId)
							body := "Build completed"
							if fail {
								body = "Build failed"
							}
							c.Assert(record.Body.GetStringValue(), qt.Equals, body)
						}
					}
				}
			}
			c.Assert(logCount, qt.Equals, 1)
		})
	}
}

func TestCachedPartialTelemetry(t *testing.T) {
	t.Parallel()
	ctx, recorder := newTelemetryRecorder(t)
	b := Test(t, `
-- hugo.toml --
disableKinds = ["taxonomy", "term", "RSS", "sitemap"]
-- layouts/home.html --
{{ partialCached "outer.html" . }}
{{ partialCached "outer.html" . }}
-- layouts/_partials/outer.html --
{{ return (partialCached "inner.html" .) }}
-- layouts/_partials/inner.html --
{{ return "cached result" }}
`, func(cfg *IntegrationTestConfig) { cfg.BuildCfg.Context = ctx })
	b.AssertFileContent("public/index.html", "cached result\ncached result")
	partials := make(map[string]sdktrace.ReadOnlySpan)
	var page sdktrace.ReadOnlySpan
	waits := 0
	for _, span := range recorder.Ended() {
		switch span.Name() {
		case "hugo.page.render":
			page = span
		case "hugo.partial.execute":
			for _, a := range span.Attributes() {
				if a.Key == "hugo.partial.name" {
					partials[a.Value.AsString()] = span
				}
			}
		case "hugo.partial.cache_wait":
			waits++
		}
	}
	b.Assert(waits, qt.Equals, 0)
	b.Assert(partials, qt.HasLen, 2)
	b.Assert(page, qt.IsNotNil)
	outer := partials["_partials/outer.html"]
	inner := partials["_partials/inner.html"]
	b.Assert(outer, qt.IsNotNil)
	b.Assert(inner, qt.IsNotNil)
	b.Assert(outer.Parent().SpanID(), qt.Equals, page.SpanContext().SpanID())
	b.Assert(inner.Parent().SpanID(), qt.Equals, outer.SpanContext().SpanID())
	counts := make(map[string]int)
	for _, span := range recorder.Ended() {
		counts[span.Name()]++
	}
	b.Assert(counts["hugo.partial.execute"], qt.Equals, 2)
}

func TestSassTelemetry(t *testing.T) {
	t.Setenv("HUGO_NUMWORKERMULTIPLIER", "4")
	if !dartsass.Supports() {
		t.Skip("Dart Sass not available")
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			ctx, recorder := newTelemetryRecorder(t)
			files := `
-- hugo.toml --
disableKinds = ["taxonomy", "term", "RSS", "sitemap"]
-- layouts/home.html --
{{ partialCached "css.html" . }} {{ partialCached "css.html" . }}
-- layouts/_partials/css.html --
{{ $css := resources.Get "main.scss" | css.Sass (dict "outputStyle" "compressed") }}
{{ return $css.RelPermalink }}
-- assets/main.scss --
$color: red; body { color: $color; }
`
			if fail {
				files = strings.Replace(files, "$color: red;", "$color red;", 1)
			} else {
				files += "\n-- layouts/page.html --\n{{ partialCached `css.html` . }}\n"
				for i := range 16 {
					files += fmt.Sprintf("\n-- content/p%d.md --\n---\ntitle: P%d\n---\n", i, i)
				}
			}
			b, err := TestE(t, files, TestOptOsFs(), func(cfg *IntegrationTestConfig) { cfg.BuildCfg.Context = ctx })
			if fail {
				b.Assert(err, qt.IsNotNil)
			} else {
				b.Assert(err, qt.IsNil)
				b.AssertFileContent("public/main.css", "body{color:red}")
			}
			var sass, partial sdktrace.ReadOnlySpan
			count := 0
			for _, span := range recorder.Ended() {
				switch span.Name() {
				case "hugo.sass.transform":
					sass = span
					count++
				case "hugo.partial.execute":
					partial = span
				}
			}
			b.Assert(count, qt.Equals, 1)
			b.Assert(sass, qt.IsNotNil)
			b.Assert(partial, qt.IsNotNil)
			b.Assert(sass.Parent().SpanID(), qt.Equals, partial.SpanContext().SpanID())
			b.Assert(sass.SpanContext().TraceID(), qt.Equals, partial.SpanContext().TraceID())
			if fail {
				b.Assert(sass.Status().Code, qt.Equals, codes.Error)
				b.Assert(partial.Status().Code, qt.Equals, codes.Error)
			} else {
				b.Assert(sass.Status().Code, qt.Equals, codes.Unset)
				b.Assert(partial.Status().Code, qt.Equals, codes.Unset)
				waits := 0
				for _, span := range recorder.Ended() {
					if span.Name() == "hugo.partial.cache_wait" {
						waits++
						b.Assert(span.Links(), qt.HasLen, 1)
						b.Assert(span.Links()[0].SpanContext.Equal(partial.SpanContext()), qt.IsTrue)
						b.Assert(span.SpanContext().TraceID(), qt.Equals, partial.SpanContext().TraceID())
						b.Assert(span.EndTime().Before(span.StartTime()), qt.IsFalse)
					}
				}
				b.Assert(waits > 0, qt.IsTrue)
			}
		})
	}
}

func newTelemetryRecorder(t *testing.T) (context.Context, *tracetest.SpanRecorder) {
	t.Helper()
	c := qt.New(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { c.Check(provider.Shutdown(context.Background()), qt.IsNil) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "parent")
	t.Cleanup(func() { parent.End() })
	return ctx, recorder
}

func TestBuildTelemetryRebuild(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { c.Check(provider.Shutdown(context.Background()), qt.IsNil) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()
	b := Test(t, `
-- hugo.toml --
disableKinds = ["taxonomy", "term", "RSS", "sitemap"]
-- content/p1.md --
---
title: P1
---
First build.
-- layouts/page.html --
{{ .Content }}
-- layouts/list.html --
{{ .Title }}
`, TestOptWatching(), func(cfg *IntegrationTestConfig) { cfg.BuildCfg.Context = ctx })
	b.EditFileReplaceAll("content/p1.md", "First build.", "Second build.").Build()
	b.AssertFileContent("public/p1/index.html", "Second build.")
	var rebuilds []bool
	for _, span := range recorder.Ended() {
		c.Assert(span.SpanContext().TraceID(), qt.Equals, parent.SpanContext().TraceID())
		if span.Name() == "hugo.build" {
			c.Assert(span.Parent().SpanID(), qt.Equals, parent.SpanContext().SpanID())
			for _, a := range span.Attributes() {
				if a.Key == "hugo.build.rebuild" {
					rebuilds = append(rebuilds, a.Value.AsBool())
				}
			}
		}
	}
	c.Assert(rebuilds, qt.DeepEquals, []bool{false, true})
}
