package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Every phase start is logged, and the final line lists each phase once, in
// first-run order, with repeated phases added together.
func TestPhaseTimerDurations(t *testing.T) {
	var out bytes.Buffer
	timer := newPhaseTimer(context.Background(), slog.New(slog.NewJSONHandler(&out, nil)), 0, "Sync progress")
	timer.begin("read_source", "Reading", "scans", 1)
	timer.begin("fsck", "Checking")
	timer.begin("read_source", "Reading again")
	timer.finish("Sync phase durations")
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], `"scans":1`) {
		t.Fatalf("unexpected log:\n%s", out.String())
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[3]), &last); err != nil {
		t.Fatal(err)
	}
	if last["msg"] != "Sync phase durations" || last["read_source"] == nil || last["fsck"] == nil || strings.Index(lines[3], "read_source") > strings.Index(lines[3], "fsck") {
		t.Fatalf("unexpected durations: %s", lines[3])
	}
	// Runs without a logger have no timer; its methods must be no-ops.
	var none *phaseTimer
	none.begin("read_source", "Reading")
	none.finish("done")
}
