package fileedit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ProgressPrefix marks a progress line: how many of an upload's or an unzip's
// bytes are in so far, as JSON. Those two run as Jobs felis-api does not wait on,
// and the panel shows how far one has got by reading the latest such line from
// the tail of the Pod's log. Like ResultPrefix it is found by its marker, since
// the log is stdout and stderr merged.
const ProgressPrefix = "FELIS-FILES-PROGRESS: "

// Progress is one progress line.
type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// ThrottledProgress returns a progress func that prints to w at most once per
// every, plus the first call (the last print starts at the zero time) and the
// one that reaches the total, so the log grows by a line a second however fast
// the bytes move and still ends on the true final count. A write error is
// dropped: progress is a courtesy, and the result line that follows is what
// felis-api acts on.
func ThrottledProgress(w io.Writer, every time.Duration, now func() time.Time) func(done, total int64) {
	var last time.Time
	return func(done, total int64) {
		t := now()
		if done < total && t.Sub(last) < every {
			return
		}
		last = t
		b, _ := json.Marshal(Progress{Done: done, Total: total})
		fmt.Fprintf(w, "%s%s\n", ProgressPrefix, b)
	}
}

// lastProgress finds the latest well-formed progress line in a log.
func lastProgress(log string) (Progress, bool) {
	var p Progress
	found := false
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 0, 4096), maxLogBytes)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), ProgressPrefix)
		if !ok {
			continue
		}
		var q Progress
		if json.Unmarshal([]byte(rest), &q) == nil {
			p, found = q, true
		}
	}
	return p, found
}
