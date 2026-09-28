package fileedit

import (
	"bytes"
	"testing"
	"time"
)

func TestThrottledProgress(t *testing.T) {
	var out bytes.Buffer
	clock := time.Unix(1000, 0)
	progress := ThrottledProgress(&out, time.Second, func() time.Time { return clock })

	progress(0, 100)  // first call: printed
	progress(10, 100) // same instant: dropped
	clock = clock.Add(999 * time.Millisecond)
	progress(20, 100) // not a second yet: dropped
	clock = clock.Add(time.Millisecond)
	progress(30, 100)  // a second on: printed
	progress(100, 100) // the total, however soon: printed
	want := ProgressPrefix + `{"done":0,"total":100}` + "\n" +
		ProgressPrefix + `{"done":30,"total":100}` + "\n" +
		ProgressPrefix + `{"done":100,"total":100}` + "\n"
	if out.String() != want {
		t.Fatalf("printed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestLastProgress(t *testing.T) {
	log := "noise\n" +
		ProgressPrefix + `{"done":1,"total":9}` + "\n" +
		"a runtime warning on stderr\n" +
		ProgressPrefix + `{"done":5,"total":9}` + "\n" +
		ProgressPrefix + `{"done":` + "\n" // cut off mid-line by the tail
	p, ok := lastProgress(log)
	if !ok || p != (Progress{Done: 5, Total: 9}) {
		t.Fatalf("lastProgress = %+v, %v; want {5 9}, true", p, ok)
	}
	if _, ok := lastProgress("no marker here\n"); ok {
		t.Fatal("a log without a progress line reported one")
	}
}
