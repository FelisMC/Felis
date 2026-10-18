package api

import (
	"encoding/json"
	"testing"
	"time"

	"felis.lolicon.best/internal/updates"
)

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
