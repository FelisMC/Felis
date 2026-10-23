package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
)

const consoleHistoryRoute = "/api/v1/servers/survival/console/history"
const emptyLogDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func newHistoryAPI(t *testing.T) (*API, *fakeRepo, *fakeCluster, *fakeFileEditor) {
	t.Helper()
	a, repo, cl, files := mkFiles(t)
	a.External = staticExternal{p: fileOwner}
	files.logTail = fileedit.LogTailPage{Content: []byte{}, ContentSHA256: emptyLogDigest}
	return a, repo, cl, files
}

func TestConsoleHistoryAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, server, owner string
		principal           *Principal
		status              int
	}{
		{"owner", "survival", "owner1", fileOwner, 200},
		{"administrator", "survival", "someone-else", fileAdmin, 200},
		{"stranger", "survival", "owner1", fileStranger, 403},
		{"unowned", "survival", "", fileOwner, 403},
		{"administrator without admin access", "survival", "owner1", &Principal{UserID: "admin1", Role: "admin"}, 403},
		{"signed out", "survival", "owner1", nil, 401},
		{"unknown server", "missing", "owner1", fileOwner, 404},
		{"invalid name", "Invalid", "owner1", fileOwner, 400},
		{"system administrator", "login", "", fileAdmin, 200},
		{"system server player", "login", "", fileOwner, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, files := newHistoryAPI(t)
			a.External = staticExternal{p: tc.principal}
			repo.byName["survival"].OwnerID = tc.owner
			cl.byName["login"] = &ServerInfo{Name: "login", Phase: "Stopped", DesiredState: "Stopped", ReaperExempt: true}
			w := do(a.ExternalHandler(), "GET", strings.Replace(consoleHistoryRoute, "survival", tc.server, 1), "", nil)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), tc.status)
			}
			if tc.status == 200 {
				if files.calls != 1 || files.gotServer != tc.server || files.gotOp != "log-tail" {
					t.Fatalf("executor calls=%d server=%q operation=%q", files.calls, files.gotServer, files.gotOp)
				}
			} else if files.calls != 0 {
				t.Fatalf("a refused request reached the file executor %d times", files.calls)
			}
		})
	}
}

func TestConsoleHistoryRequiresFullyStoppedServer(t *testing.T) {
	for _, tc := range []struct {
		name, phase, desired string
		ready, allowed       bool
	}{
		{"stopped", "Stopped", "Stopped", false, true},
		{"failed and stopped", "Failed", "Stopped", false, true},
		{"unknown and stopped", "Unknown", "Stopped", false, true},
		{"running", "Running", "Running", true, false},
		{"running before scale-down", "Running", "Stopped", false, false},
		{"starting", "Starting", "Running", false, false},
		{"cancelled start", "Starting", "Stopped", false, false},
		{"stopping", "Stopping", "Stopped", false, false},
		{"wake requested", "Stopped", "Running", false, false},
		{"failed retry", "Failed", "Running", false, false},
		{"remaining ready pod", "Stopped", "Stopped", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, cl, files := newHistoryAPI(t)
			info := cl.byName["survival"]
			info.Phase, info.DesiredState, info.Ready = tc.phase, tc.desired, tc.ready
			w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
			if tc.allowed {
				if w.Code != http.StatusOK || files.calls != 1 || len(cl.acquired) != 0 || len(cl.released) != 0 {
					t.Fatalf("read acquired a write lock or failed: status=%d calls=%d acquired=%v released=%v body=%s", w.Code, files.calls, cl.acquired, cl.released, w.Body.String())
				}
			} else if w.Code != http.StatusConflict || decodeErr(t, w) != "server_transitioning" || files.calls != 0 {
				t.Fatalf("unsafe lifecycle: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
			}
		})
	}
}

func TestConsoleHistoryMaintenanceGate(t *testing.T) {
	for _, kind := range []string{maintenance.KindBackup, maintenance.KindRestore, maintenance.KindMigration, maintenance.KindReap} {
		t.Run(kind, func(t *testing.T) {
			a, _, cl, files := newHistoryAPI(t)
			cl.fileAccessErr = &MaintenanceBusyError{Kind: kind}
			cl.noWorld["survival"] = true
			w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
			if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" || files.calls != 0 || len(cl.ensured) != 0 {
				t.Fatalf("maintenance admitted history: status=%d calls=%d ensured=%v body=%s", w.Code, files.calls, cl.ensured, w.Body.String())
			}
		})
	}
	for _, kind := range []string{maintenance.KindFileWrite, maintenance.KindConfigWrite, maintenance.KindExport} {
		t.Run(kind+" permits reads", func(t *testing.T) {
			a, _, cl, files := newHistoryAPI(t)
			// CheckFileAccess permits reads while these holders still reject a new write lock.
			cl.maintErr["survival"] = &MaintenanceBusyError{Kind: kind}
			w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
			if w.Code != http.StatusOK || files.calls != 1 || len(cl.acquired) != 0 || len(cl.ensured) != 0 {
				t.Fatalf("read used a write gate: status=%d calls=%d acquired=%v ensured=%v body=%s", w.Code, files.calls, cl.acquired, cl.ensured, w.Body.String())
			}
		})
	}
}

func TestConsoleHistoryMissingVolumeHasNoSideEffects(t *testing.T) {
	a, repo, cl, files := newHistoryAPI(t)
	cl.noWorld["survival"] = true
	a.Files = nil
	w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
	want := map[string]any{"content": "", "content_sha256": emptyLogDigest, "has_more": false, "byte_limited": false}
	if w.Code != http.StatusOK || !reflect.DeepEqual(fileAnswer(t, w), want) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing volume: status=%d body=%s", w.Code, w.Body.String())
	}
	if len(cl.ensured) != 0 || !cl.noWorld["survival"] || files.calls != 0 || len(cl.acquired) != 0 || len(repo.audits) != 0 {
		t.Fatalf("history created state: ensured=%v noWorld=%v calls=%d acquired=%v audits=%v", cl.ensured, cl.noWorld, files.calls, cl.acquired, repo.audits)
	}
	w = do(a.ExternalHandler(), "GET", consoleHistoryRoute+"?cursor=previous-page", "", nil)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "file_changed" || files.calls != 0 || len(cl.ensured) != 0 {
		t.Fatalf("missing volume accepted an old cursor: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestConsoleHistoryPageContract(t *testing.T) {
	a, _, _, files := newHistoryAPI(t)
	content := []byte{0xe4, 0xb8, '\n'}
	sum := sha256.Sum256(content)
	files.logTail = fileedit.LogTailPage{Content: content, ContentSHA256: hex.EncodeToString(sum[:]), NextCursor: "older-page", HasMore: true, ByteLimited: true}
	w := do(a.ExternalHandler(), "GET", consoleHistoryRoute+"?cursor=current-page", "", nil)
	want := map[string]any{
		"content": base64.StdEncoding.EncodeToString(content), "content_sha256": hex.EncodeToString(sum[:]),
		"next_cursor": "older-page", "has_more": true, "byte_limited": true,
	}
	if w.Code != http.StatusOK || !reflect.DeepEqual(fileAnswer(t, w), want) || files.gotCursor != "current-page" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("page changed in transit: status=%d cursor=%q body=%s", w.Code, files.gotCursor, w.Body.String())
	}
	files.logTail = fileedit.LogTailPage{ContentSHA256: emptyLogDigest}
	w = do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
	if w.Code != http.StatusOK || !reflect.DeepEqual(fileAnswer(t, w), map[string]any{"content": "", "content_sha256": emptyLogDigest, "has_more": false, "byte_limited": false}) {
		t.Fatalf("empty page was not normalized: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestConsoleHistoryCursorBound(t *testing.T) {
	for _, cursor := range []string{strings.Repeat("a", 257), strings.Repeat("界", 86)} {
		a, _, _, files := newHistoryAPI(t)
		w := do(a.ExternalHandler(), "GET", consoleHistoryRoute+"?cursor="+url.QueryEscape(cursor), "", nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_path" || files.calls != 0 {
			t.Fatalf("oversized cursor reached executor: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
		}
	}
	a, _, _, files := newHistoryAPI(t)
	cursor := strings.Repeat("a", 256)
	w := do(a.ExternalHandler(), "GET", consoleHistoryRoute+"?cursor="+cursor, "", nil)
	if w.Code != http.StatusOK || files.gotCursor != cursor || files.calls != 1 {
		t.Fatalf("cursor upper bound was not delegated: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
	}
}

func TestConsoleHistoryExecutorErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"malformed cursor", fileedit.ErrBadPath, 400, "bad_path"},
		{"protected alias", fmt.Errorf("%w: managed configuration", fileedit.ErrBadPath), 400, "bad_path"},
		{"replaced log", fileedit.ErrConflict, 409, "file_changed"},
		{"damaged page", fileedit.ErrReadDamaged, 502, "read_damaged"},
		{"timeout", context.DeadlineExceeded, 504, "files_timeout"},
		{"executor failure", errors.New("executor unavailable"), 500, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, files := newHistoryAPI(t)
			files.err = tc.err
			w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
			if w.Code != tc.status || decodeErr(t, w) != tc.code {
				t.Fatalf("status=%d body=%s, want %d/%s", w.Code, w.Body.String(), tc.status, tc.code)
			}
		})
	}
	t.Run("unconfigured file editor", func(t *testing.T) {
		a, _, _, _ := newHistoryAPI(t)
		a.Files = nil
		w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "files_unavailable" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})
}

type historyExecuteRunner struct {
	fileedit.Runner
	root  string
	calls int
}

func (runner *historyExecuteRunner) Run(_ context.Context, p fileedit.JobParams) ([]byte, error) {
	runner.calls++
	res, err := fileedit.Execute(runner.root, fileedit.Request{Op: p.Op, Cursor: p.Cursor})
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func TestConsoleHistoryReadsLargeStoredLogWithoutLiveServices(t *testing.T) {
	a, _, cl, _ := newHistoryAPI(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "logs/latest.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var tail strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&tail, "stored line %03d\n", i)
	}
	if _, err := f.WriteAt([]byte("\n"+tail.String()), fileedit.MaxReadBytes+1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &historyExecuteRunner{root: root}
	a.Files = &fileedit.Editor{Runner: runner, Config: fileedit.Config{Image: "test-image"}}
	streamer := &fakeLogStreamer{err: ErrConsoleUnavailable}
	a.Logs, a.Console = streamer, nil
	w := do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
	var page fileedit.LogTailPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || string(page.Content) != tail.String() || !page.HasMore || page.NextCursor == "" || page.ByteLimited {
		t.Fatalf("stored tail: status=%d bytes=%d page=%+v", w.Code, len(page.Content), page)
	}
	if runner.calls != 1 || streamer.calls != 0 || len(cl.acquired) != 0 || len(cl.ensured) != 0 {
		t.Fatalf("stored read touched live services: jobs=%d streams=%d acquired=%v ensured=%v", runner.calls, streamer.calls, cl.acquired, cl.ensured)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	w = do(a.ExternalHandler(), "GET", consoleHistoryRoute+"?cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "file_changed" {
		t.Fatalf("removed log accepted an old cursor: status=%d body=%s", w.Code, w.Body.String())
	}
	w = do(a.ExternalHandler(), "GET", consoleHistoryRoute, "", nil)
	if w.Code != http.StatusOK || !reflect.DeepEqual(fileAnswer(t, w), map[string]any{"content": "", "content_sha256": emptyLogDigest, "has_more": false, "byte_limited": false}) {
		t.Fatalf("missing log did not return an empty page: status=%d body=%s", w.Code, w.Body.String())
	}
}
