package fileedit

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeRunner stands in for the cluster: it records the JobParams the Editor
// rendered and replays a canned payload as if a Pod had printed it.
type fakeRunner struct {
	calls   int
	got     []JobParams
	payload []byte
	err     error

	started  []JobParams
	startErr error
	ops      []OpState
	opsArgs  [][2]string
}

func (f *fakeRunner) Start(_ context.Context, p JobParams) error {
	f.started = append(f.started, p)
	return f.startErr
}

func (f *fakeRunner) Ops(_ context.Context, namespace, server string) ([]OpState, error) {
	f.opsArgs = append(f.opsArgs, [2]string{namespace, server})
	return f.ops, f.err
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
		r := &fakeRunner{payload: mustPayload(t, Result{Entries: []Entry{{Name: "a"}}, Avail: 7 << 30})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}

		ls, err := e.List(context.Background(), "survival", "config")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ls.Entries) != 1 || ls.Truncated || ls.Free != 7<<30 {
			t.Fatalf("listing = %+v", ls)
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
		r := &fakeRunner{payload: mustPayload(t, Result{Content: []byte("motd=hi\n"), SHA256: "abc", ContentSHA256: hex.EncodeToString(sumOf("motd=hi\n"))})}
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

		sum, err := e.Write(context.Background(), "survival", "ops.json", []byte("[]"), "old", false)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if r.got[0].Op != OpWrite || string(r.got[0].Content) != "[]" || r.got[0].Expect != "old" ||
			r.got[0].CreateOnly || sum != "new" {
			t.Fatalf("params = %+v, sha256 = %q", r.got[0], sum)
		}
	})

	t.Run("create-only write", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{SHA256: "new"})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		if _, err := e.Write(context.Background(), "survival", "new.yml", nil, "", true); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !r.got[0].CreateOnly {
			t.Fatalf("params = %+v, want CreateOnly", r.got[0])
		}
	})

	t.Run("mkdir, delete and rename", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		ctx := context.Background()
		if err := e.Mkdir(ctx, "survival", "plugins"); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := e.Delete(ctx, "survival", "old.jar"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := e.Rename(ctx, "survival", "a.txt", "b.txt"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		for i, want := range []struct{ op, path, to string }{
			{OpMkdir, "plugins", ""}, {OpDelete, "old.jar", ""}, {OpRename, "a.txt", "b.txt"},
		} {
			p := r.got[i]
			if p.Op != want.op || p.Path != want.path || p.To != want.to || p.Server != "survival" || p.WorldPVC != "world-survival-0" {
				t.Errorf("call %d params = %+v, want %+v", i, p, want)
			}
		}
	})

	t.Run("upload", func(t *testing.T) {
		r := &fakeRunner{payload: mustPayload(t, Result{})}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		src := UploadSource{URL: "http://api/x", Token: "tok", Size: 42, SHA256: "sum"}
		if err := e.Upload(context.Background(), "survival", "plugins/x.jar", src, true); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		p := r.got[0]
		if p.Op != OpUpload || p.Path != "plugins/x.jar" || p.SourceURL != "http://api/x" || p.UploadToken != "tok" ||
			p.UploadSize != 42 || p.UploadSHA256 != "sum" || !p.Overwrite {
			t.Fatalf("params = %+v", p)
		}
	})
}

// TestEditorMintsAFreshOpID guards the RBAC-forced invariant from the other side:
// the Job name is unique per invocation only because the Editor mints a new id
// every time. If it ever cached one, two operations would collide on a name
// felis-api has no permission to delete.
func TestEditorMintsAFreshOpID(t *testing.T) {
	r := &fakeRunner{payload: mustPayload(t, Result{ContentSHA256: hex.EncodeToString(sumOf(""))})}
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
		{"already there", CodeExists, ErrExists},
		{"changed on the way", CodeDigestMismatch, ErrDigestMismatch},
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

	_, err := e.Write(context.Background(), "survival", "big.txt", make([]byte, MaxWriteBytes+1), "", false)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if r.calls != 0 {
		t.Fatal("an oversized write must never reach the cluster")
	}
}

// A read whose bytes do not hash to the digest the Job sent with them changed
// on the way, and none of them is handed on: an editor saving a damaged read
// would write the damage back.
func TestEditorReadRefusesBytesChangedOnTheWay(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent string
	}{
		{"hashed otherwise", hex.EncodeToString(sumOf("motd=hi\n"))},
		{"with no digest", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{payload: mustPayload(t, Result{Content: []byte("motd=ho\n"), SHA256: "abc", ContentSHA256: tc.sent})}
			e := &Editor{Runner: r, Config: Config{Image: "img"}}
			got, sum, err := e.Read(context.Background(), "survival", "server.properties")
			if !errors.Is(err, ErrReadDamaged) || got != nil || sum != "" {
				t.Fatalf("Read = %q, %q, %v; want nothing and ErrReadDamaged", got, sum, err)
			}
		})
	}
}

// TestEditorNormalisesEmptyResults pins that "nothing there" is a success, not a
// nil surprise: an empty directory lists as [] and a zero-length file reads as
// empty bytes, so no caller has to distinguish nil from empty.
func TestEditorNormalisesEmptyResults(t *testing.T) {
	r := &fakeRunner{payload: mustPayload(t, Result{ContentSHA256: hex.EncodeToString(sumOf(""))})}
	e := &Editor{Runner: r, Config: Config{Image: "img"}}

	ls, err := e.List(context.Background(), "survival", "empty")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ls.Entries == nil {
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

// TestEditorStartsBackgroundOps checks an upload or unzip too long to wait on is
// started, not run: its Job carries the async label and the longer deadline,
// the longer TTL Ops reads it back within, and the larger CPU share, and what
// comes back names the Job Ops will report on.
func TestEditorStartsBackgroundOps(t *testing.T) {
	src := UploadSource{URL: "http://api/big", Token: "tok", Size: 5 << 30, SHA256: "sum"}

	t.Run("upload", func(t *testing.T) {
		r := &fakeRunner{}
		e := &Editor{Runner: r, Config: Config{Image: "img", Namespace: "mc"}}
		before := time.Now()
		st, err := e.StartUpload(context.Background(), "survival", "maps/world.zip", src, true)
		if err != nil {
			t.Fatalf("StartUpload: %v", err)
		}
		if r.calls != 0 || len(r.started) != 1 {
			t.Fatalf("ran %d, started %d; want the one Job started and none waited on", r.calls, len(r.started))
		}
		p := r.started[0]
		if !p.Async || p.Op != OpUpload || p.Path != "maps/world.zip" || !p.Overwrite ||
			p.SourceURL != src.URL || p.UploadToken != "tok" || p.UploadSize != 5<<30 || p.UploadSHA256 != "sum" {
			t.Fatalf("params = %+v", p)
		}
		if p.Deadline != 2*time.Hour || p.TTLAfterFinished != 30*time.Minute || p.CPULimit != "1" || p.MemLimit != "256Mi" {
			t.Fatalf("deadline %v ttl %v cpu %q mem %q, want 2h 30m 1 256Mi",
				p.Deadline, p.TTLAfterFinished, p.CPULimit, p.MemLimit)
		}
		if p.Namespace != "mc" || p.Server != "survival" || p.WorldPVC != "world-survival-0" || p.Image != "img" || p.OpID == "" {
			t.Fatalf("params = %+v", p)
		}
		if st.ID != p.OpID || st.Op != OpUpload || st.Path != "maps/world.zip" || st.State != OpRunning || st.Started.Before(before) {
			t.Fatalf("state = %+v, want the started Job %s running", st, p.OpID)
		}

		if _, err := e.StartUpload(context.Background(), "survival", "maps/world.zip", src, false); err != nil || r.started[1].Overwrite {
			t.Fatalf("an upload that must not replace a file started with %+v (%v)", r.started[1], err)
		}
	})

	t.Run("unzip, with its own limits", func(t *testing.T) {
		r := &fakeRunner{}
		e := &Editor{Runner: r, Config: Config{Image: "img", AsyncDeadline: time.Hour, AsyncTTL: time.Minute, AsyncCPULimit: "2"}}
		st, err := e.StartUnzip(context.Background(), "survival", "maps/world.zip", false)
		if err != nil {
			t.Fatalf("StartUnzip: %v", err)
		}
		p := r.started[0]
		if !p.Async || p.Op != OpUnzip || p.Path != "maps/world.zip" || p.Overwrite || p.SourceURL != "" {
			t.Fatalf("params = %+v", p)
		}
		if p.Deadline != time.Hour || p.TTLAfterFinished != time.Minute || p.CPULimit != "2" {
			t.Fatalf("deadline %v ttl %v cpu %q, want the configured 1h 1m 2", p.Deadline, p.TTLAfterFinished, p.CPULimit)
		}
		if st.ID != p.OpID || st.Op != OpUnzip {
			t.Fatalf("state = %+v", st)
		}
	})

	t.Run("a Job that could not be created", func(t *testing.T) {
		boom := errors.New("forbidden")
		r := &fakeRunner{startErr: boom}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		st, err := e.StartUnzip(context.Background(), "survival", "a.zip", false)
		if !errors.Is(err, boom) || st != (OpState{}) {
			t.Fatalf("state %+v err %v, want nothing started and %v", st, err, boom)
		}
	})

	t.Run("ops", func(t *testing.T) {
		want := []OpState{{ID: "0a", State: OpRunning}}
		r := &fakeRunner{ops: want}
		e := &Editor{Runner: r, Config: Config{Image: "img"}}
		got, err := e.Ops(context.Background(), "survival")
		if err != nil || len(got) != 1 || got[0] != want[0] {
			t.Fatalf("Ops = %+v %v", got, err)
		}
		if r.opsArgs[0] != [2]string{"minecraft", "survival"} {
			t.Fatalf("asked %v, want the default namespace and the server", r.opsArgs[0])
		}
	})
}
