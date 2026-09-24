package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeRunner stands in for the cluster: it records the JobParams the Editor
// rendered and replays a canned payload as if a Pod had printed it.
type fakeRunner struct {
	calls   int
	got     []JobParams
	payload []byte
	err     error
}

func (f *fakeRunner) Run(_ context.Context, p JobParams) ([]byte, error) {
	f.calls++
	f.got = append(f.got, p)
	return f.payload, f.err
}

func mustPayload(t *testing.T, res Result) []byte {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestEditorRendersParams checks the Editor projects each operation onto the
// JobParams the renderer expects — in particular that the world PVC comes from the
// shared naming convention rather than being assembled locally, which is what keeps
// the editor pointed at the same volume the operator created and the reaper deletes.
func TestEditorRendersParams(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{Entries: []Entry{{Name: "a"}}})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}

		entries, truncated, err := e.List(context.Background(), "survival", "config")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) != 1 || truncated {
			t.Fatalf("entries=%+v truncated=%v", entries, truncated)
		}
		p := r.got[0]
		if p.Op != OpList || p.Path != "config" || p.Server != "survival" {
			t.Fatalf("params = %+v", p)
		}
		if p.WorldPVC != "world-survival-0" {
			t.Fatalf("WorldPVC = %q, want the naming convention's world-survival-0", p.WorldPVC)
		}
		if p.WorldsRoot != "/data" {
			t.Fatalf("WorldsRoot = %q, want the default /data", p.WorldsRoot)
		}
		if len(p.Content) != 0 {
			t.Fatal("a list must carry no content")
		}
	})

	t.Run("read", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{Content: []byte("motd=hi\n"), SHA256: "abc"})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}

		got, sum, err := e.Read(context.Background(), "survival", "server.properties")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(got) != "motd=hi\n" || sum != "abc" {
			t.Fatalf("content = %q sha256 = %q", got, sum)
		}
		if r.got[0].Op != OpRead || r.got[0].Path != "server.properties" {
			t.Fatalf("params = %+v", r.got[0])
		}
	})

	t.Run("write", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{SHA256: "new"})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}

		sum, err := e.Write(context.Background(), "survival", "ops.json", []byte("[]"), "old")
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if r.got[0].Op != OpWrite || string(r.got[0].Content) != "[]" || r.got[0].Expect != "old" || sum != "new" {
			t.Fatalf("params = %+v, sha256 = %q", r.got[0], sum)
		}
	})
}

// TestEditorMintsAFreshOpID guards the RBAC-forced invariant from the other side:
// the Job name is unique per invocation only because the Editor mints a new id
// every time. If it ever cached one, two operations would collide on a name
// felis-api has no permission to delete.
func TestEditorMintsAFreshOpID(t *testing.T) {
	r := &fakeRunner{payload: mustPayload(t, Result{})}
	e := &Editor{Runner: r, Config: Config{Image: "img"}}

	for range 3 {
		if _, _, err := e.Read(context.Background(), "survival", "x"); err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	seen := map[string]bool{}
	for _, p := range r.got {
		if p.OpID == "" {
			t.Fatal("op id must never be empty")
		}
		if seen[p.OpID] {
			t.Fatalf("op id %q reused across invocations", p.OpID)
		}
		seen[p.OpID] = true
	}
}

// TestEditorMapsResultCodes proves a caller-fault Result becomes the sentinel the
// API maps. The default branch matters most: an unrecognised code must FAIL rather
// than read as success, so a future Job version reporting a new failure mode cannot
// be silently mistaken for a completed operation.
func TestEditorMapsResultCodes(t *testing.T) {
	cases := []struct {
		name string
		code string
		want error
	}{
		{"missing file", CodeNotFound, ErrNotFound},
		{"escaping path", CodeBadPath, ErrBadPath},
		{"oversized", CodeTooLarge, ErrTooLarge},
		{"changed since read", CodeConflict, ErrConflict},
		{"volume full", CodeNoSpace, ErrNoSpace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{payload: mustPayload(t, Result{Code: tc.code, Error: "detail here"})}
			e := &Editor{Runner: r, Config: Config{Image: "img"}}
			_, _, err := e.Read(context.Background(), "survival", "x")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("an unknown code still fails", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{Code: "from_the_future", Error: "?"})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		if _, _, err := e.Read(context.Background(), "survival", "x"); err == nil {
			t.Fatal("an unrecognised failure code must not read as success")
		}
	})

	t.Run("a malformed payload is an error, not an empty success", func(t *testing.T) {
		r := &fakeRunner{payload: []byte("not json at all")}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		if _, _, err := e.Read(context.Background(), "survival", "x"); err == nil {
			t.Fatal("a malformed result must fail")
		}
	})

	t.Run("a runner failure propagates", func(t *testing.T) {
		r := &fakeRunner{err: errors.New("pod never scheduled")}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		if _, _, err := e.Read(context.Background(), "survival", "x"); err == nil {
			t.Fatal("a runner error must propagate")
		}
	})
}

// TestEditorRefusesOversizedWriteBeforeTheCluster checks the size ceiling is applied
// before a Job is rendered. Letting it through would surface as an opaque etcd
// object-size rejection long after felis-api committed to the request.
func TestEditorRefusesOversizedWriteBeforeTheCluster(t *testing.T) {
	r := &fakeRunner{payload: mustPayload(t, Result{})}
	e := &Editor{Runner: r, Config: Config{Image: "img"}}

	_, err := e.Write(context.Background(), "survival", "big.txt", make([]byte, MaxWriteBytes+1), "")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if r.calls != 0 {
		t.Fatal("an oversized write must never reach the cluster")
	}
}

// TestEditorNormalisesEmptyResults pins that "nothing there" is a success, not a
// nil surprise: an empty directory lists as [] and a zero-length file reads as
// empty bytes, so no caller has to distinguish nil from empty.
func TestEditorNormalisesEmptyResults(t *testing.T) {
	r := &fakeRunner{payload: mustPayload(t, Result{})}
	e := &Editor{Runner: r, Config: Config{Image: "img"}}

	entries, _, err := e.List(context.Background(), "survival", "empty")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if entries == nil {
		t.Fatal("an empty directory must list as [], not nil")
	}

	content, _, err := e.Read(context.Background(), "survival", "empty.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content == nil {
		t.Fatal("a zero-length file must read as empty bytes, not nil")
	}
}

// TestExtractResult covers the log-scanning half of the transport: pods/log merges
// stdout and stderr, so the payload must be found by its marker among arbitrary
// noise rather than by assuming the log is pure JSON.
func TestExtractResult(t *testing.T) {
	t.Run("finds the payload among stderr noise", func(t *testing.T) {
		log := "warning: something from the runtime\n" +
			ResultPrefix + `{"content":"aGk="}` + "\n" +
			"a trailing stderr line\n"
		payload, ok := extractResult(log)
		if !ok {
			t.Fatal("payload not found")
		}
		var res Result
		if err := json.Unmarshal(payload, &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if string(res.Content) != "hi" {
			t.Fatalf("content = %q", res.Content)
		}
	})

	t.Run("takes the last marked line", func(t *testing.T) {
		log := ResultPrefix + `{"code":"bad_path"}` + "\n" + ResultPrefix + `{"content":"aGk="}` + "\n"
		payload, ok := extractResult(log)
		if !ok {
			t.Fatal("payload not found")
		}
		if string(payload) != `{"content":"aGk="}` {
			t.Fatalf("payload = %s, want the last marked line", payload)
		}
	})

	t.Run("reports absence rather than guessing", func(t *testing.T) {
		if _, ok := extractResult("no marker here\njust noise\n"); ok {
			t.Fatal("a log with no marked line must report not-found")
		}
	})
}
