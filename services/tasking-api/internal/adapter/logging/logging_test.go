package logging_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/mhayk/overpass/services/tasking-api/internal/adapter/logging"
)

func TestLevelMapping(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		if got := logging.Level(name); got != want {
			t.Errorf("%q mapped to %v, want %v", name, got, want)
		}
	}
}

func TestAnUnknownLevelFallsBackToInfoAndNotToError(t *testing.T) {
	// Too many logs is a nuisance. Too few is an investigation that cannot
	// happen, so the fallback goes to the noisier option deliberately.
	if got := logging.Level("chatty"); got != slog.LevelInfo {
		t.Fatalf("got %v, want info", got)
	}
}

func TestOutputIsJSONAndCarriesServiceAndVersion(t *testing.T) {
	var buf strings.Builder
	logging.New(&buf, "tasking-api", "v9", "info").Info("hello")

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
		t.Fatalf("output was not JSON: %v\n%s", err, buf.String())
	}
	if entry["service"] != "tasking-api" || entry["version"] != "v9" {
		t.Fatalf("service and version missing: %v", entry)
	}
}

func TestTheLevelIsActuallyApplied(t *testing.T) {
	var buf strings.Builder
	logging.New(&buf, "s", "v", "error").Info("should not appear")
	if buf.Len() != 0 {
		t.Fatalf("an info line was emitted at error level: %s", buf.String())
	}
}
