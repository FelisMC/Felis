package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestConsoleCommand(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	// mk builds an API whose "survival" server is owned by owner1 and Ready, with a
	// fresh fakeConsole wired. Subtests override only what they need.
	mk := func() (*API, *fakeRepo, *fakeCluster, *fakeConsole) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running", Ready: true, DesiredState: "Running"}
		console := &fakeConsole{reply: "There are 3 of a max of 20 players online"}
		api := newTestAPI(repo, cl)
		api.Console = console
		return api, repo, cl, console
	}

	t.Run("owner runs command -> 200 + reply + audit", func(t *testing.T) {
		api, repo, _, console := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var resp struct {
			Name      string `json:"name"`
			Output    string `json:"output"`
			Transport string `json:"transport"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.Name != "survival" || resp.Output != console.reply || resp.Transport != "rcon" {
			t.Fatalf("unexpected response %+v", resp)
		}
		if console.gotName != "survival" || console.gotCommand != "list" {
			t.Fatalf("console saw (%q,%q), want (survival,list)", console.gotName, console.gotCommand)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "console.command" || repo.audits[0].Actor != "owner1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})

	t.Run("starting container does not require readiness", func(t *testing.T) {
		api, _, cl, console := mk()
		cl.byName["survival"].Phase, cl.byName["survival"].Ready = "Starting", false
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusOK || console.calls != 1 || len(cl.ensured) != 0 || len(cl.acquired) != 0 {
			t.Fatalf("startup input: status=%d calls=%d ensured=%v lock=%v body=%s", w.Code, console.calls, cl.ensured, cl.acquired, w.Body.String())
		}
	})

	t.Run("stop requested is rejected even while ready", func(t *testing.T) {
		api, _, cl, console := mk()
		cl.byName["survival"].DesiredState = "Stopped"
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_running" || console.calls != 0 || len(cl.ensured) != 0 {
			t.Fatalf("stopping input: status=%d calls=%d body=%s", w.Code, console.calls, w.Body.String())
		}
	})

	t.Run("maintenance does not create a volume or send a command", func(t *testing.T) {
		api, _, cl, console := mk()
		cl.fileAccessErr = &MaintenanceBusyError{Kind: "backup"}
		cl.noWorld["survival"] = true
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" || console.calls != 0 || len(cl.ensured) != 0 || len(cl.acquired) != 0 {
			t.Fatalf("maintenance input: status=%d calls=%d ensured=%v lock=%v body=%s", w.Code, console.calls, cl.ensured, cl.acquired, w.Body.String())
		}
	})

	t.Run("lost reply is unknown and never says to retry blindly", func(t *testing.T) {
		api, repo, _, console := mk()
		console.err = ErrConsoleOutcomeUnknown
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusBadGateway || decodeErr(t, w) != "command_outcome_unknown" || console.calls != 1 || len(repo.audits) != 1 || repo.audits[0].Action != "console.command.uncertain" {
			t.Fatalf("uncertain command: status=%d calls=%d audits=%v body=%s", w.Code, console.calls, repo.audits, w.Body.String())
		}
	})

	t.Run("leading slash stripped before RCON", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"/say hi"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.gotCommand != "say hi" {
			t.Fatalf("console got %q, want %q", console.gotCommand, "say hi")
		}
	})

	t.Run("admin runs command on another's server -> 200", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 1 {
			t.Fatalf("admin command reached RCON %d times, want 1", console.calls)
		}
	})

	t.Run("non-owner -> 403, no RCON call", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("a forbidden caller must not reach RCON")
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/missing/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("stopped phase -> 409 not_running, no console call", func(t *testing.T) {
		api, _, cl, console := mk()
		cl.byName["survival"].Ready = false
		cl.byName["survival"].Phase = "Stopped"
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_running" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 0 {
			t.Fatal("a stopped server must not reach the console")
		}
	})

	t.Run("empty command -> 400, no RCON call", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"   "}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("an empty command must not reach RCON")
		}
	})

	t.Run("slash-only command -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"/"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	t.Run("control character (newline) -> 400, no RCON call", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: owner}
		// The \n is a JSON escape that decodes to a real newline; the handler must
		// reject it so one request cannot smuggle a second command.
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command",
			`{"command":"say hi\nop attacker"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 0 {
			t.Fatal("a control-char command must not reach RCON")
		}
	})

	t.Run("over-long command -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		long := strings.Repeat("a", maxConsoleCommandLen+1)
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command",
			`{"command":"`+long+`"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	t.Run("unknown field -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command",
			`{"command":"list","extra":1}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	t.Run("console unreachable -> 503 console_unavailable", func(t *testing.T) {
		api, repo, _, console := mk()
		console.err = ErrConsoleUnavailable
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		// A failed write must not be audited as a successful command.
		if len(repo.audits) != 0 {
			t.Fatalf("unreachable console must not audit: %+v", repo.audits)
		}
	})

	t.Run("nil Console -> 503", func(t *testing.T) {
		api, _, _, _ := mk()
		api.Console = nil
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
}

func TestConsoleCommandStartupSubmitsStdinWithoutRCON(t *testing.T) {
	k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
	if err := k.c.Get(context.Background(), client.ObjectKeyFromObject(ms), ms); err != nil {
		t.Fatal(err)
	}
	ms.Status.Phase = v1alpha1.PhaseStarting
	if err := k.c.Status().Update(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	calls, submitted := 0, ""
	k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) {
		calls++
		return consoleExecutorFunc(func(_ context.Context, o remotecommand.StreamOptions) error {
			line, err := io.ReadAll(o.Stdin)
			submitted = string(line)
			return err
		}), nil
	}
	repo, cl := newFakeRepo(), newFakeCluster()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Starting", DesiredState: "Running"}
	a := newTestAPI(repo, cl)
	a.Console = k
	a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
	w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"/list"}`, nil)
	var got ConsoleCommandResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body=%s: %v", w.Body.String(), err)
	}
	if w.Code != http.StatusOK || got.Name != "survival" || got.Transport != "stdin" || got.Output != "" || submitted != "list\n" || calls != 1 || len(cl.ensured) != 0 || len(cl.acquired) != 0 {
		t.Fatalf("startup submission: status=%d result=%+v input=%q calls=%d ensured=%v locks=%v", w.Code, got, submitted, calls, cl.ensured, cl.acquired)
	}
}
