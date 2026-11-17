package commands

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	qt "github.com/frankban/quicktest"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestBuildTelemetryLogs(t *testing.T) {
	c := qt.New(t)
	dir := t.TempDir()
	for name, content := range map[string]string{
		"hugo.toml":         `disableKinds = ["taxonomy", "term", "RSS", "sitemap"]`,
		"content/p1.md":     "---\ntitle: P1\n---\nHello.",
		"layouts/page.html": `{{ warnf "telemetry warning" }}{{ .Content }}`,
		"layouts/list.html": `{{ .Title }}`,
	} {
		filename := filepath.Join(dir, name)
		c.Assert(os.MkdirAll(filepath.Dir(filename), 0o755), qt.IsNil)
		c.Assert(os.WriteFile(filename, []byte(content), 0o600), qt.IsNil)
	}
	var mu sync.Mutex
	var payloads [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/v1/logs" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		payloads = append(payloads, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(server.Close)
	t.Setenv("HUGO_OTEL_ENABLED", "true")
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL+"/v1/logs")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "none")
	c.Assert(Execute([]string{"--source", dir}), qt.IsNil)
	mu.Lock()
	defer mu.Unlock()
	bodies := make(map[string]bool)
	for _, b := range payloads {
		var req collectorlog.ExportLogsServiceRequest
		c.Assert(proto.Unmarshal(b, &req), qt.IsNil)
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, record := range sl.LogRecords {
					bodies[record.Body.GetStringValue()] = true
				}
			}
		}
	}
	c.Assert(bodies["telemetry warning"], qt.IsTrue)
	c.Assert(bodies["Build completed"], qt.IsTrue)
}

func TestBuildTelemetryCommandSpan(t *testing.T) {
	for _, test := range []struct {
		name     string
		layout   string
		exitCode int64
	}{
		{"success", `{{ .Content }}`, 0},
		{"failure", `{{ errorf "boom" }}`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			dir := t.TempDir()
			for name, content := range map[string]string{
				"hugo.toml":         `disableKinds = ["taxonomy", "term", "RSS", "sitemap"]`,
				"content/p1.md":     "---\ntitle: P1\n---\nHello.",
				"layouts/page.html": test.layout,
				"layouts/list.html": `{{ .Title }}`,
			} {
				filename := filepath.Join(dir, name)
				c.Assert(os.MkdirAll(filepath.Dir(filename), 0o755), qt.IsNil)
				c.Assert(os.WriteFile(filename, []byte(content), 0o600), qt.IsNil)
			}
			var mu sync.Mutex
			var payloads [][]byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil || r.URL.Path != "/v1/traces" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				payloads = append(payloads, b)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			t.Cleanup(server.Close)
			t.Setenv("HUGO_OTEL_ENABLED", "true")
			t.Setenv("OTEL_SDK_DISABLED", "false")
			t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
			t.Setenv("OTEL_METRICS_EXPORTER", "none")
			t.Setenv("OTEL_LOGS_EXPORTER", "none")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "none")
			t.Setenv("TRACEPARENT", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

			err := Execute([]string{"--source", dir})
			c.Assert(err != nil, qt.Equals, test.exitCode != 0)

			mu.Lock()
			defer mu.Unlock()
			var span *tracev1.Span
			var scope *tracev1.ScopeSpans
			for _, b := range payloads {
				var req collectortrace.ExportTraceServiceRequest
				c.Assert(proto.Unmarshal(b, &req), qt.IsNil)
				for _, rs := range req.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						for _, s := range ss.Spans {
							if s.Name == "hugo.command.build" {
								span, scope = s, ss
							}
						}
					}
				}
			}
			c.Assert(span, qt.IsNotNil)
			c.Assert(hex.EncodeToString(span.TraceId), qt.Equals, "0af7651916cd43dd8448eb211c80319c")
			c.Assert(hex.EncodeToString(span.ParentSpanId), qt.Equals, "b7ad6b7169203331")
			c.Assert(scope.SchemaUrl, qt.Matches, `https://opentelemetry.io/schemas/.+`)
			c.Assert(scope.Scope.Version, qt.Not(qt.Equals), "")
			attrs := make(map[string]*commonv1.AnyValue)
			for _, kv := range span.Attributes {
				attrs[kv.Key] = kv.Value
			}
			c.Assert(attrs["process.pid"].GetIntValue(), qt.Equals, int64(os.Getpid()))
			c.Assert(attrs["process.executable.name"].GetStringValue(), qt.Not(qt.Equals), "")
			c.Assert(attrs["process.exit.code"].GetIntValue(), qt.Equals, test.exitCode)
			if test.exitCode == 0 {
				c.Assert(attrs["error.type"], qt.IsNil)
			} else {
				c.Assert(attrs["error.type"].GetStringValue(), qt.Not(qt.Equals), "")
			}
		})
	}
}
