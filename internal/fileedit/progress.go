package fileedit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ProgressPrefix identifies JSON progress updates in merged stdout and stderr.
const ProgressPrefix = "FELIS-FILES-PROGRESS: "

type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// ThrottledProgress emits the first, interval-spaced and final counts; progress-write failures do not fail the operation.
func ThrottledProgress(w io.Writer, every time.Duration, now func() time.Time) func(done, total int64) {
	var last time.Time
	return func(done, total int64) {
		t := now()
		if (total <= 0 || done < total) && t.Sub(last) < every {
			return
		}
		last = t
		b, _ := json.Marshal(Progress{Done: done, Total: total})
		fmt.Fprintf(w, "%s%s\n", ProgressPrefix, b)
	}
}

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
