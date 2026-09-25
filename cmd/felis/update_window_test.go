package main

import (
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/updates"
)

func TestRenderWindowLinePlacesNowAgainstTheWindow(t *testing.T) {
	prev := time.Local
	time.Local = time.FixedZone("CST", 8*3600)
	t.Cleanup(func() { time.Local = prev })

	w := updates.Window{
		Start: time.Date(2026, 9, 26, 18, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 9, 26, 20, 0, 0, 0, time.UTC),
	}
	cases := []struct {
		name string
		w    updates.Window
		err  error
		now  time.Time
		want string
	}{
		{"unreadable", updates.Window{}, errors.New("connection refused"), w.Start, "Maintenance window: unknown (connection refused).\n"},
		{"unset", updates.Window{}, nil, w.Start, "Maintenance window: not set; apply whenever suits you.\n"},
		{"half set", updates.Window{Start: w.Start}, nil, w.Start, "Maintenance window: not set; apply whenever suits you.\n"},
		{"at the opening instant", w, nil, w.Start, "Maintenance window: open now, until 2026-09-27 04:00 CST.\n"},
		{"before", w, nil, w.Start.Add(-time.Minute), "Maintenance window: opens 2026-09-27 02:00 CST, until 2026-09-27 04:00 CST. Felis applies nothing on its own; run the apply commands inside it.\n"},
		{"at the closing instant", w, nil, w.End, "Maintenance window: ended 2026-09-27 04:00 CST; set a new one in the panel before applying.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderWindowLine(tc.w, tc.err, tc.now); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
