package telemetry

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
)

func TestDisabled(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "sdk_disabled"}[disabled], func(t *testing.T) {
			c := qt.New(t)
			t.Setenv("HUGO_OTEL_ENABLED", "false")
			t.Setenv("OTEL_SDK_DISABLED", "false")
			if disabled {
				t.Setenv("HUGO_OTEL_ENABLED", "true")
				t.Setenv("OTEL_SDK_DISABLED", "TRUE")
			}
			// Disabled telemetry must ignore even an invalid exporter configuration.
			t.Setenv("OTEL_TRACES_EXPORTER", "invalid")
			ctx := context.Background()
			got, shutdown, err := FromEnv(ctx, "test")
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, ctx)
			c.Assert(Enabled(got), qt.IsFalse)
			c.Assert(shutdown(), qt.IsNil)
			c.Assert(shutdown(), qt.IsNil)
		})
	}
}
