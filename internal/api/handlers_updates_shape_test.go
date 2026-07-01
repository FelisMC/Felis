package api

import (
	"encoding/json"
	"testing"
	"time"

	"felis.lolicon.best/internal/updates"
)

// TestUpdateWindowStorageShapeDecodesIntoCoreWindow is a cross-package contract
// guard. The maintenance window THIS package persists (api.updateWindow, lowercase
// {"start","end"}) must decode straight into the update decision core's
// updates.Window — because the (INTEGRATION-ONLY) `felis update` runner reads the
// stored bytes back into a Window. The test marshals the REAL api DTO rather than a
// hand-written JSON literal (which would drift silently if either shape changed) and
// unmarshals into updates.Window, proving the on-disk bytes the admin API writes are
// exactly what the runner will read back — no silent zero-window from a key-casing
// mismatch. updates.Window carries json:"start"/json:"end" tags precisely so this
// holds; without them the natural Unmarshal would zero every field.
func TestUpdateWindowStorageShapeDecodesIntoCoreWindow(t *testing.T) {
	start := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC)

	raw, err := json.Marshal(updateWindow{Start: &start, End: &end})
	if err != nil {
		t.Fatalf("marshal api window: %v", err)
	}

	var win updates.Window
	if err := json.Unmarshal(raw, &win); err != nil {
		t.Fatalf("unmarshal into updates.Window: %v", err)
	}
	if !win.Start.Equal(start) || !win.End.Equal(end) {
		t.Fatalf("decoded window = {%s,%s}, want {%s,%s}", win.Start, win.End, start, end)
	}
	mid := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	if !win.Contains(mid) {
		t.Fatalf("Contains(%s) = false, want true — the window did not survive the round-trip", mid)
	}

	// A cleared/never-set window ({null,null}) must decode to the zero Window, which
	// fails closed (Contains always false) — never a spurious open apply slot.
	rawEmpty, err := json.Marshal(updateWindow{})
	if err != nil {
		t.Fatalf("marshal empty api window: %v", err)
	}
	var empty updates.Window
	if err := json.Unmarshal(rawEmpty, &empty); err != nil {
		t.Fatalf("unmarshal empty into updates.Window: %v", err)
	}
	if !empty.Start.IsZero() || !empty.End.IsZero() {
		t.Fatalf("empty window decoded to non-zero {%s,%s}", empty.Start, empty.End)
	}
	if empty.Contains(mid) {
		t.Fatal("zero window Contains returned true, want false (must fail closed)")
	}
}
