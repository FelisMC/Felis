package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
	"go.yaml.in/yaml/v3"
)

const paperGlobalPath = "/api/v1/servers/survival/config/paper-global"

var paperGlobalPatchBody = `{"values":{"collisions.enable-player-collisions":false},"expect_sha256":"` + testSum + `"}`

func TestPaperGlobalRequiresAuthenticatedOwnerOrStaffSession(t *testing.T) {
	for _, method := range []string{"GET", "PATCH"} {
		for _, tc := range []struct {
			name      string
			principal *Principal
			status    int
		}{
			{"signed out", nil, http.StatusUnauthorized},
			{"admin without staff session", &Principal{UserID: "admin1", Role: "admin"}, http.StatusForbidden},
		} {
			t.Run(method+" "+tc.name, func(t *testing.T) {
				a, _, _, files := mkFiles(t)
				a.External = staticExternal{p: tc.principal}
				body := ""
				if method == "PATCH" {
					body = paperGlobalPatchBody
				}
				w := do(a.ExternalHandler(), method, paperGlobalPath, body, fileRouteHeader("paper-global", body))
				if w.Code != tc.status || files.calls != 0 {
					t.Fatalf("session gate: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
				}
			})
		}
	}
}

func TestPaperGlobalRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]any
	}{
		{"scalars", map[string]any{
			"collisions.enable-player-collisions":            false,
			"player-auto-save.rate":                          float64(6000),
			"chunk-loading-basic.player-max-chunk-send-rate": 75.5,
			"messages.no-permission":                         "<red>Contact the server owner",
		}},
		{"missing values remain absent", nil},
		{"old values remain readable", map[string]any{"watchdog.early-warning-delay": float64(-20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, files := mkFiles(t)
			a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
			files.paperGlobal = fileedit.PaperGlobalConfig{Values: tc.values, SHA256: testSum}
			w := do(a.ExternalHandler(), "GET", paperGlobalPath, "", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("read: status=%d body=%s", w.Code, w.Body.String())
			}
			values := tc.values
			if values == nil {
				values = map[string]any{}
			}
			if got := fileAnswer(t, w); !reflect.DeepEqual(got, map[string]any{"values": values, "sha256": testSum}) {
				t.Fatalf("configuration response = %#v", got)
			}
			if files.calls != 1 || files.gotOp != fileedit.OpPaperGlobalRead || files.gotServer != "survival" {
				t.Fatalf("wrong read: %+v", files)
			}
			if w.Header().Get("Cache-Control") != "no-store" || len(repo.audits) != 0 || len(cl.acquired) != 0 {
				t.Fatalf("read side effects: headers=%v audit=%v locks=%v", w.Header(), repo.audits, cl.acquired)
			}
		})
	}
}

func TestPaperGlobalPatch(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			a, repo, cl, files := mkFiles(t)
			a.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}
			if running {
				cl.byName["survival"].Phase = "Running"
				cl.byName["survival"].Ready = true
				cl.byName["survival"].DesiredState = "Running"
			}
			files.paperGlobal = fileedit.PaperGlobalConfig{
				Values: map[string]any{
					"collisions.enable-player-collisions": false,
					"watchdog.early-warning-delay":        float64(-20),
				},
				SHA256: strings.Repeat("b", 64),
			}
			w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, paperGlobalPatchBody, jsonHeader)
			if w.Code != http.StatusOK {
				t.Fatalf("patch: status=%d body=%s", w.Code, w.Body.String())
			}
			if files.gotOp != fileedit.OpPaperGlobalPatch || files.gotServer != "survival" || files.gotExpect != testSum ||
				!reflect.DeepEqual(files.gotValues, map[string]any{"collisions.enable-player-collisions": false}) {
				t.Fatalf("patch must pass only explicitly changed fields: %+v", files)
			}
			if got := fileAnswer(t, w); !reflect.DeepEqual(got, map[string]any{"values": files.paperGlobal.Values, "sha256": files.paperGlobal.SHA256}) {
				t.Fatalf("saved configuration = %#v", got)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("configuration responses must not be cached")
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "config.paper-global.patch" ||
				repo.audits[0].ServerName != "survival:config/paper-global.yml" || repo.audits[0].Actor != "owner1@example.net" ||
				len(repo.audits[0].Payload) != 0 {
				t.Fatalf("patch audit must not contain values: %+v", repo.audits)
			}
			if running {
				if len(cl.acquired) != 0 || len(cl.released) != 0 {
					t.Fatalf("stable running configuration took maintenance lock: %v %v", cl.acquired, cl.released)
				}
			} else if !reflect.DeepEqual(cl.acquired, []string{"survival:" + maintenance.KindFileWrite}) ||
				!reflect.DeepEqual(cl.released, []string{"survival"}) {
				t.Fatalf("stopped configuration lock: %v %v", cl.acquired, cl.released)
			}
		})
	}
	t.Run("empty patch is explicit and allowed", func(t *testing.T) {
		a, _, _, files := mkFiles(t)
		a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
		w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, `{"values":{},"expect_sha256":"`+testSum+`"}`, jsonHeader)
		if w.Code != http.StatusOK || files.gotValues == nil || len(files.gotValues) != 0 || files.gotExpect != testSum {
			t.Fatalf("empty patch: status=%d values=%v body=%s", w.Code, files.gotValues, w.Body.String())
		}
	})
}

func TestPaperGlobalPatchValidation(t *testing.T) {
	const fakeSecret = "fake-private-forwarding-value"
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing values", `{"expect_sha256":"` + testSum + `"}`},
		{"null values", `{"values":null,"expect_sha256":"` + testSum + `"}`},
		{"array values", `{"values":[],"expect_sha256":"` + testSum + `"}`},
		{"missing hash", `{"values":{}}`},
		{"empty hash", `{"values":{},"expect_sha256":""}`},
		{"uppercase hash", `{"values":{},"expect_sha256":"` + strings.Repeat("A", 64) + `"}`},
		{"short hash", `{"values":{},"expect_sha256":"abcd"}`},
		{"path is unsupported", `{"values":{},"expect_sha256":"` + testSum + `","path":"` + fakeSecret + `"}`},
		{"raw YAML is unsupported", `{"values":{},"expect_sha256":"` + testSum + `","yaml":"` + fakeSecret + `"}`},
		{"unknown field name is safe", `{"values":{},"expect_sha256":"` + testSum + `","` + fakeSecret + `":true}`},
		{"unknown ID", `{"values":{"` + fakeSecret + `":true},"expect_sha256":"` + testSum + `"}`},
		{"proxy secret", `{"values":{"proxies.velocity.secret":"` + fakeSecret + `"},"expect_sha256":"` + testSum + `"}`},
		{"proxy enabled", `{"values":{"proxies.velocity.enabled":false},"expect_sha256":"` + testSum + `"}`},
		{"nested proxies", `{"values":{"proxies":{"velocity":{"secret":"` + fakeSecret + `"}}},"expect_sha256":"` + testSum + `"}`},
		{"null scalar", `{"values":{"collisions.enable-player-collisions":null},"expect_sha256":"` + testSum + `"}`},
		{"string instead of boolean", `{"values":{"collisions.enable-player-collisions":"` + fakeSecret + `"},"expect_sha256":"` + testSum + `"}`},
		{"fractional integer", `{"values":{"player-auto-save.rate":1.5},"expect_sha256":"` + testSum + `"}`},
		{"negative watchdog", `{"values":{"watchdog.early-warning-delay":-1},"expect_sha256":"` + testSum + `"}`},
		{"overflow integer", `{"values":{"player-auto-save.rate":2147483648},"expect_sha256":"` + testSum + `"}`},
		{"invalid negative rate", `{"values":{"chunk-loading-basic.player-max-chunk-send-rate":-0.5},"expect_sha256":"` + testSum + `"}`},
		{"overflow number", `{"values":{"chunk-loading-basic.player-max-chunk-send-rate":1e999},"expect_sha256":"` + testSum + `"}`},
		{"NUL string", `{"values":{"messages.no-permission":"\u0000"},"expect_sha256":"` + testSum + `"}`},
		{"long UTF-8 string", `{"values":{"messages.no-permission":"` + strings.Repeat("世", 1366) + `"},"expect_sha256":"` + testSum + `"}`},
		{"malformed JSON", `{"values":{"proxies.velocity.secret":"` + fakeSecret + `"}`},
		{"trailing JSON", paperGlobalPatchBody + `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, files := mkFiles(t)
			a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
			w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, tc.body, jsonHeader)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" ||
				files.calls != 0 || len(cl.acquired) != 0 || len(repo.audits) != 0 || strings.Contains(w.Body.String(), fakeSecret) {
				t.Fatalf("invalid patch reached configuration or exposed input: status=%d calls=%d locks=%v audit=%v body=%s",
					w.Code, files.calls, cl.acquired, repo.audits, w.Body.String())
			}
		})
	}
	t.Run("64 KiB bound applies before validation", func(t *testing.T) {
		a, repo, cl, files := mkFiles(t)
		a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
		body := `{"values":{"messages.no-permission":"` + strings.Repeat("x", fileedit.MaxPaperGlobalPatchBytes) + `"},"expect_sha256":"` + testSum + `"}`
		w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, body, jsonHeader)
		if w.Code != http.StatusRequestEntityTooLarge || decodeErr(t, w) != "too_large" || files.calls != 0 || len(cl.acquired) != 0 || len(repo.audits) != 0 {
			t.Fatalf("oversized patch: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
		}
	})
}

func TestPaperGlobalErrorsDoNotExposeConfiguration(t *testing.T) {
	const fakeSecret = "fake-private-forwarding-value"
	for _, method := range []string{"GET", "PATCH"} {
		for _, tc := range []struct {
			name   string
			err    error
			status int
			code   string
		}{
			{"missing file", fileedit.ErrNotFound, http.StatusNotFound, "not_found"},
			{"unsafe YAML or link", fileedit.ErrBadPath, http.StatusBadRequest, "bad_path"},
			{"oversized YAML", fileedit.ErrTooLarge, http.StatusRequestEntityTooLarge, "too_large"},
			{"changed file", fileedit.ErrConflict, http.StatusConflict, "file_changed"},
			{"full volume", fileedit.ErrNoSpace, http.StatusInsufficientStorage, "volume_full"},
			{"damaged patch", fileedit.ErrDigestMismatch, http.StatusBadRequest, "digest_mismatch"},
			{"damaged result", fileedit.ErrReadDamaged, http.StatusBadGateway, "read_damaged"},
			{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout, "files_timeout"},
			{"unexpected diagnostic", fmt.Errorf("parser %s", fakeSecret), http.StatusInternalServerError, "internal"},
		} {
			if method == "GET" && (tc.err == fileedit.ErrNoSpace || tc.err == fileedit.ErrDigestMismatch) {
				continue
			}
			t.Run(method+" "+tc.name, func(t *testing.T) {
				a, repo, cl, files := mkFiles(t)
				a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
				files.err = fmt.Errorf("YAML scalar %s: %w", fakeSecret, tc.err)
				body := ""
				if method == "PATCH" {
					body = paperGlobalPatchBody
				}
				w := do(a.ExternalHandler(), method, paperGlobalPath, body, fileRouteHeader("paper-global", body))
				if w.Code != tc.status || decodeErr(t, w) != tc.code || strings.Contains(w.Body.String(), fakeSecret) || len(repo.audits) != 0 {
					t.Fatalf("unsafe error mapping: status=%d body=%s audits=%v", w.Code, w.Body.String(), repo.audits)
				}
				if method == "PATCH" && !reflect.DeepEqual(cl.released, []string{"survival"}) {
					t.Fatalf("failed patch did not release its lock: %v", cl.released)
				}
			})
		}
	}
}

func TestPaperGlobalPatchLockConflict(t *testing.T) {
	a, repo, cl, files := mkFiles(t)
	a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
	cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindFileWrite}
	w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, paperGlobalPatchBody, jsonHeader)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" || files.calls != 0 || len(repo.audits) != 0 {
		t.Fatalf("overlapping save: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
	}
}

func TestPaperGlobalPatchWholeBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"inclusive limit", strings.Repeat(" ", fileedit.MaxPaperGlobalPatchBytes-len(paperGlobalPatchBody)) + paperGlobalPatchBody, http.StatusOK},
		{"oversized trailing whitespace", paperGlobalPatchBody + strings.Repeat(" ", fileedit.MaxPaperGlobalPatchBytes), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, files := mkFiles(t)
			a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
			w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, tc.body, jsonHeader)
			if w.Code != tc.status || (files.calls == 1) != (tc.status == http.StatusOK) {
				t.Fatalf("whole body bound: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
			}
		})
	}
}

func TestPaperGlobalScalarPatchWireTypes(t *testing.T) {
	a, _, _, files := mkFiles(t)
	a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
	values := map[string]any{
		"collisions.enable-player-collisions":            true,
		"chunk-loading-basic.player-max-chunk-send-rate": 12.5,
		"player-auto-save.rate":                          float64(-1),
		"messages.no-permission":                         "<gold>权限不足",
	}
	body, err := json.Marshal(paperGlobalPatchRequest{Values: values, ExpectSHA256: testSum})
	if err != nil {
		t.Fatal(err)
	}
	w := do(a.ExternalHandler(), "PATCH", paperGlobalPath, string(body), jsonHeader)
	if w.Code != http.StatusOK || !reflect.DeepEqual(files.gotValues, values) {
		t.Fatalf("scalar values changed in transit: status=%d values=%#v body=%s", w.Code, files.gotValues, w.Body.String())
	}
}

type paperGlobalExecuteRunner struct {
	fileedit.Runner
	root string
}

func (runner *paperGlobalExecuteRunner) Run(_ context.Context, p fileedit.JobParams) ([]byte, error) {
	sum := sha256.Sum256(p.Content)
	res, err := fileedit.Execute(runner.root, fileedit.Request{
		Op: p.Op, Path: p.Path, Content: p.Content, Expect: p.Expect, ContentSHA256: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func TestPaperGlobalRoundTripKeepsSecretsInWorker(t *testing.T) {
	const fakeSecret = "fake-shared-forwarding-value"
	const original = "# global configuration\ncollisions:\n  enable-player-collisions: true # keep this comment\nwatchdog:\n  early-warning-delay: -20\ncustom-setting:\n  nested: [one, two]\nproxies:\n  velocity:\n    enabled: true\n    online-mode: true\n    secret: " + fakeSecret + "\n"
	a, repo, _, _ := mkFiles(t)
	a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config/paper-global.yml")
	if err := os.WriteFile(configPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	a.Files = &fileedit.Editor{Runner: &paperGlobalExecuteRunner{root: root}, Config: fileedit.Config{Image: "test-image"}}
	h := a.ExternalHandler()
	w := do(h, "GET", paperGlobalPath, "", nil)
	var opened fileedit.PaperGlobalConfig
	if err := json.Unmarshal(w.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), fakeSecret) || !reflect.DeepEqual(opened.Values, map[string]any{
		"collisions.enable-player-collisions": true,
		"watchdog.early-warning-delay":        json.Number("-20"),
	}) {
		t.Fatalf("filtered read: status=%d body=%s", w.Code, w.Body.String())
	}
	originalSum := sha256.Sum256([]byte(original))
	if opened.SHA256 != hex.EncodeToString(originalSum[:]) {
		t.Fatalf("read hash must cover the complete YAML: %s", opened.SHA256)
	}
	for _, id := range []string{"console.enable-brigadier-completions", "collisions.send-full-pos-for-hard-colliding-entities"} {
		missingFieldPatch := `{"values":{"` + id + `":false},"expect_sha256":"` + opened.SHA256 + `"}`
		w = do(h, "PATCH", paperGlobalPath, missingFieldPatch, jsonHeader)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_path" || strings.Contains(w.Body.String(), fakeSecret) || len(repo.audits) != 0 {
			t.Fatalf("missing field %s patch: status=%d body=%s audits=%v", id, w.Code, w.Body.String(), repo.audits)
		}
		if after, err := os.ReadFile(configPath); err != nil || string(after) != original {
			t.Fatalf("missing field created a mapping or changed the original YAML: %q err=%v", after, err)
		}
	}
	patch := func(expect string) string {
		return `{"values":{"collisions.enable-player-collisions":false},"expect_sha256":"` + expect + `"}`
	}
	changed := original + "# concurrent external edit\n"
	if err := os.WriteFile(configPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	w = do(h, "PATCH", paperGlobalPath, patch(opened.SHA256), jsonHeader)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "file_changed" || strings.Contains(w.Body.String(), fakeSecret) || len(repo.audits) != 0 {
		t.Fatalf("stale patch: status=%d body=%s audits=%v", w.Code, w.Body.String(), repo.audits)
	}
	if after, err := os.ReadFile(configPath); err != nil || string(after) != changed {
		t.Fatalf("stale save overwrote the file: %q err=%v", after, err)
	}
	w = do(h, "GET", paperGlobalPath, "", nil)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &opened) != nil {
		t.Fatalf("refresh: status=%d body=%s", w.Code, w.Body.String())
	}
	w = do(h, "PATCH", paperGlobalPath, patch(opened.SHA256), jsonHeader)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), fakeSecret) || len(repo.audits) != 1 {
		t.Fatalf("save: status=%d body=%s audits=%v", w.Code, w.Body.String(), repo.audits)
	}
	var saved fileedit.PaperGlobalConfig
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Values["collisions.enable-player-collisions"] != false || saved.Values["watchdog.early-warning-delay"] != json.Number("-20") {
		t.Fatalf("save changed unrelated old values: %#v", saved.Values)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	savedSum := sha256.Sum256(after)
	if saved.SHA256 != hex.EncodeToString(savedSum[:]) {
		t.Fatalf("saved hash must cover the complete YAML: %s", saved.SHA256)
	}
	var beforeDoc, afterDoc map[string]any
	if err := yaml.Unmarshal([]byte(changed), &beforeDoc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(after, &afterDoc); err != nil {
		t.Fatal(err)
	}
	beforeDoc["collisions"].(map[string]any)["enable-player-collisions"] = false
	if !reflect.DeepEqual(beforeDoc, afterDoc) || !strings.Contains(string(after), "# global configuration") ||
		!strings.Contains(string(after), "# keep this comment") || !strings.Contains(string(after), "# concurrent external edit") {
		t.Fatalf("worker changed unknown/proxy values or comments: %s", after)
	}
	for _, path := range []string{"config/paper-global.yml", "secret-alias.yml"} {
		if path == "secret-alias.yml" {
			if err := os.Link(configPath, filepath.Join(root, path)); err != nil {
				t.Fatal(err)
			}
		}
		w = do(h, "GET", "/api/v1/servers/survival/file?path="+path, "", nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_path" || strings.Contains(w.Body.String(), fakeSecret) {
			t.Fatalf("ordinary file read exposed protected path %s: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	hardLinkPatch := `{"values":{"collisions.enable-player-collisions":true},"expect_sha256":"` + saved.SHA256 + `"}`
	w = do(h, "PATCH", paperGlobalPath, hardLinkPatch, jsonHeader)
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_path" || strings.Contains(w.Body.String(), fakeSecret) || len(repo.audits) != 1 {
		t.Fatalf("hard-linked secret file was replaced: status=%d body=%s audits=%v", w.Code, w.Body.String(), repo.audits)
	}
	if current, err := os.ReadFile(configPath); err != nil || string(current) != string(after) {
		t.Fatalf("hard-link refusal changed the file: %q err=%v", current, err)
	}
	if err := os.Remove(filepath.Join(root, "secret-alias.yml")); err != nil {
		t.Fatal(err)
	}
	unsafe := "proxies:\n  velocity:\n    secret: " + fakeSecret + "\ncollisions:\n  enable-player-collisions: *" + fakeSecret + "\n"
	if err := os.WriteFile(configPath, []byte(unsafe), 0644); err != nil {
		t.Fatal(err)
	}
	w = do(h, "GET", paperGlobalPath, "", nil)
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_path" || strings.Contains(w.Body.String(), fakeSecret) {
		t.Fatalf("YAML diagnostic exposed stored scalar: status=%d body=%s", w.Code, w.Body.String())
	}
}
