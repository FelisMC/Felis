package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/netguard"
)

type fileDownloadRoundTrip func(*http.Request) (*http.Response, error)

func (fn fileDownloadRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func fileDownloadResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}
}

func TestFileManagerExtensionGates(t *testing.T) {
	for _, rt := range []struct {
		method, path, body string
	}{
		{"GET", "/api/v1/servers/survival/files/search?path=plugins&q=config", ""},
		{"POST", "/api/v1/servers/survival/files/transfer", `{"op":"copy","paths":["a"],"to":"plugins"}`},
		{"POST", "/api/v1/servers/survival/files/transfer", `{"op":"move","paths":["a"],"to":"plugins"}`},
		{"POST", "/api/v1/servers/survival/files/zip", `{"paths":["a"],"to":"files.zip"}`},
		{"POST", "/api/v1/servers/survival/files/from-url", `{"url":"https://example.net/plugin.jar","path":"plugins/a.jar"}`},
	} {
		for _, st := range []struct {
			name, phase, desired, busy string
			principal                  *Principal
			status                     int
		}{
			{"stopped owner", "Stopped", "Stopped", "", fileOwner, 0},
			{"running owner", "Running", "Running", "", fileOwner, 0},
			{"stopped administrator", "Stopped", "Stopped", "", &Principal{UserID: "admin", Role: "admin", ViaAdminAccess: true}, 0},
			{"starting", "Starting", "Running", "", fileOwner, http.StatusConflict},
			{"stopping", "Stopping", "Stopped", "", fileOwner, http.StatusConflict},
			{"backup", "Running", "Running", maintenance.KindBackup, fileOwner, http.StatusConflict},
			{"restore", "Stopped", "Stopped", maintenance.KindRestore, fileOwner, http.StatusConflict},
			{"migration", "Stopped", "Stopped", maintenance.KindMigration, fileOwner, http.StatusConflict},
			{"stranger", "Stopped", "Stopped", "", fileStranger, http.StatusForbidden},
		} {
			t.Run(rt.path+rt.body+"/"+st.name, func(t *testing.T) {
				a, _, cl, files := mkFiles(t)
				a.External = staticExternal{p: st.principal}
				cl.byName["survival"].Phase, cl.byName["survival"].DesiredState = st.phase, st.desired
				if st.busy != "" {
					cl.fileAccessErr = &MaintenanceBusyError{Kind: st.busy}
				}
				fetches := 0
				a.FileDownloadClient = &http.Client{Transport: fileDownloadRoundTrip(func(*http.Request) (*http.Response, error) {
					fetches++
					return fileDownloadResponse(200, "jar"), nil
				})}
				w := do(a.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader("", rt.body))
				if st.status != 0 {
					if w.Code != st.status || files.calls != 0 || fetches != 0 {
						t.Fatalf("denied: status=%d calls=%d downloads=%d body=%s", w.Code, files.calls, fetches, w.Body.String())
					}
					return
				}
				if w.Code/100 != 2 || files.calls != 1 {
					t.Fatalf("allowed: status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
				}
				if st.phase == "Running" && len(cl.acquired) != 0 {
					t.Fatalf("live operation acquired a world lock: %v", cl.acquired)
				}
			})
		}
	}
}

func TestFileSearchAndSelectionRequests(t *testing.T) {
	t.Run("recursive result paths and truncation", func(t *testing.T) {
		a, _, _, files := mkFiles(t)
		a.External = staticExternal{p: fileOwner}
		files.entries = []fileedit.Entry{{Name: "Essentials/config.yml"}}
		files.truncated, files.free = true, -1
		w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/files/search?path=plugins&q=config", "", nil)
		var body struct {
			Path      string
			Entries   []fileedit.Entry
			Truncated bool
			Free      *int64 `json:"free_bytes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || files.gotQuery != "config" || files.gotPath != "plugins" || body.Path != "plugins" || !body.Truncated || body.Free != nil || !reflect.DeepEqual(body.Entries, files.entries) {
			t.Fatalf("search = %s", w.Body.String())
		}
	})
	for _, op := range []string{"copy", "move", "zip"} {
		t.Run(op, func(t *testing.T) {
			a, repo, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			body := `{"paths":["plugins/a.jar","plugins/b.jar"],"to":"saved.zip","overwrite":true}`
			route := op
			if op != "zip" {
				route = "transfer"
				body = `{"op":"` + op + `",` + body[1:]
			}
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/"+route, body, jsonHeader)
			if w.Code != http.StatusAccepted || files.gotOp != op || files.gotTo != "saved.zip" || !files.gotOverwrite || !reflect.DeepEqual(files.gotPaths, []string{"plugins/a.jar", "plugins/b.jar"}) {
				t.Fatalf("selection: status=%d body=%s", w.Code, w.Body.String())
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "file."+op || repo.audits[0].ServerName != "survival:saved.zip" {
				t.Fatalf("audit = %+v", repo.audits)
			}
		})
	}
	for _, tc := range []struct {
		route, body string
	}{
		{"transfer", `{"op":"delete","paths":["a"],"to":""}`},
		{"transfer", `{"op":"copy","paths":["a"]}`},
		{"transfer", `{"op":"copy","paths":[],"to":""}`},
		{"transfer", `{"op":"move","paths":[".."],"to":""}`},
		{"transfer", `{"op":"copy","paths":["a"],"to":"../outside"}`},
		{"zip", `{"paths":["a"],"to":"bundle.tar"}`},
		{"zip", `{"paths":["a"],"to":"/outside.zip"}`},
	} {
		t.Run(tc.body, func(t *testing.T) {
			a, _, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/"+tc.route, tc.body, jsonHeader)
			if w.Code != http.StatusBadRequest || files.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
			}
		})
	}
	t.Run("selection argument budget", func(t *testing.T) {
		paths := make([]string, 500)
		for i := range paths {
			paths[i] = strings.Repeat("a", 140)
		}
		if err := selectedFilePaths(paths); err == nil {
			t.Fatal("an oversized selection was accepted")
		}
	})
}

func TestFileFromURLStagesDigestAndAuditsSource(t *testing.T) {
	for _, size := range []int64{-1, 8} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			a, repo, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			a.FileDownloadClient = &http.Client{Transport: fileDownloadRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://example.net/plugin.jar" {
					t.Fatalf("source %s", r.URL)
				}
				resp := fileDownloadResponse(200, "jarbytes")
				resp.ContentLength = size
				return resp, nil
			})}
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/from-url", `{"url":"https://example.net/plugin.jar","path":"plugins/a.jar","overwrite":true}`, jsonHeader)
			if w.Code != http.StatusAccepted || files.gotSource.Size != 8 || files.gotSource.SHA256 != hexSum([]byte("jarbytes")) || !files.gotOverwrite {
				t.Fatalf("status=%d source=%+v body=%s", w.Code, files.gotSource, w.Body.String())
			}
			u, _ := url.Parse(files.gotSource.URL)
			id := u.Path[strings.LastIndex(u.Path, "/")+1:]
			f, n, err := a.FileStage.Open(id, files.gotSource.Token)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(f)
			f.Close()
			if string(got) != "jarbytes" || n != 8 {
				t.Fatalf("staged %q (%d)", got, n)
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "file.from_url" || !strings.Contains(string(repo.audits[0].Payload), `"url":"https://example.net/plugin.jar"`) {
				t.Fatalf("audit=%+v", repo.audits)
			}
			if err := a.FileStage.Landed(id, files.gotSource.Token); err != nil {
				t.Fatal(err)
			}
			stageEmpty(t, a)
		})
	}
}

func TestFileFromURLRefusesUnsafeSourcesAndRedirects(t *testing.T) {
	for _, source := range []string{"file:///etc/passwd", "https://user:pass@example.net/a", "http://127.0.0.1/a", "http://10.0.0.1/a", "http://169.254.169.254/a", "http://[::ffff:127.0.0.1]/a", "http://localhost./a", "http://example.net:0/a"} {
		t.Run(source, func(t *testing.T) {
			a, _, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			a.FileDownloadClient = &http.Client{Transport: fileDownloadRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("unsafe source was requested")
				return nil, nil
			})}
			b, _ := json.Marshal(map[string]any{"url": source, "path": "plugins/a.jar"})
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/from-url", string(b), jsonHeader)
			if w.Code != 400 || decodeErr(t, w) != "bad_url" || files.calls != 0 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, target := range []string{"http://127.0.0.1/a", "http://169.254.169.254/a", "https://name:secret@example.net/a", "file:///etc/passwd"} {
		t.Run("redirect "+target, func(t *testing.T) {
			a, _, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			calls := 0
			a.FileDownloadClient = &http.Client{Transport: fileDownloadRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				resp := fileDownloadResponse(http.StatusFound, "")
				resp.Header.Set("Location", target)
				return resp, nil
			})}
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/from-url", `{"url":"https://example.net/a","path":"a"}`, jsonHeader)
			if w.Code != 400 || decodeErr(t, w) != "bad_url" || calls != 1 || files.calls != 0 {
				t.Fatalf("status=%d requests=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
	t.Run("resolved private address", func(t *testing.T) {
		a, _, _, files := mkFiles(t)
		a.External = staticExternal{p: fileOwner}
		a.FileDownloadClient = &http.Client{Transport: fileDownloadRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, netguard.ErrNotPublic
		})}
		w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/from-url", `{"url":"https://example.net/a","path":"a"}`, jsonHeader)
		if w.Code != 400 || decodeErr(t, w) != "bad_url" || files.calls != 0 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})
}

func TestFileFromURLCleansUpFailedDownloads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		setup    func(*API, *fakeFileEditor)
		response func() (*http.Response, error)
	}{
		{"HTTP failure", 502, nil, func() (*http.Response, error) { return fileDownloadResponse(403, "denied"), nil }},
		{"transport failure", 502, nil, func() (*http.Response, error) { return nil, errors.New("connection lost") }},
		{"short body", 400, nil, func() (*http.Response, error) {
			r := fileDownloadResponse(200, "a")
			r.ContentLength = 5
			return r, nil
		}},
		{"staging floor", 507, func(a *API, _ *fakeFileEditor) { a.FileStage.MinFree = 1 }, func() (*http.Response, error) { return fileDownloadResponse(200, "bytes"), nil }},
		{"landing admission failure", 404, func(_ *API, f *fakeFileEditor) { f.err = fileedit.ErrNotFound }, func() (*http.Response, error) { return fileDownloadResponse(200, "bytes"), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			if tc.setup != nil {
				tc.setup(a, files)
			}
			a.FileDownloadClient = &http.Client{Transport: fileDownloadRoundTrip(func(*http.Request) (*http.Response, error) { return tc.response() })}
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/from-url", `{"url":"https://example.net/a","path":"a"}`, jsonHeader)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			stageEmpty(t, a)
		})
	}
}

func TestPublicDownloadTransportChecksDialAddressAndIgnoresProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8080")
	client := newFileDownloadClient()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("public downloads use the environment proxy")
	}
	_, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if !errors.Is(err, netguard.ErrNotPublic) {
		t.Fatalf("resolved address was not refused before connecting: %v", err)
	}
}

func TestFileTextWriteAcceptsFiveMiB(t *testing.T) {
	for _, n := range []int{5 << 20, 5<<20 + 1} {
		a, _, _, files := mkFiles(t)
		a.External = staticExternal{p: fileOwner}
		files.sum = testSum
		content := strings.Repeat("x", n)
		body := `{"content":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `","content_sha256":"` + hexSum([]byte(content)) + `"}`
		w := do(a.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=config.yml", body, jsonHeader)
		if n == 5<<20 && (w.Code != 200 || len(files.gotContent) != n) {
			t.Fatalf("5 MiB write: status=%d bytes=%d body=%s", w.Code, len(files.gotContent), w.Body.String())
		}
		if n > 5<<20 && (w.Code != 413 || files.calls != 0) {
			t.Fatalf("oversized write: status=%d calls=%d", w.Code, files.calls)
		}
	}
}

func TestExtractTarRequests(t *testing.T) {
	for _, name := range []string{"a.tar", "a.tar.gz", "a.tgz", "A.TAR.GZ"} {
		a, _, _, files := mkFiles(t)
		a.External = staticExternal{p: fileOwner}
		w := do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/files/unzip?path="+name, `{}`, jsonHeader)
		if w.Code != http.StatusAccepted || files.gotPath != name || files.gotOp != fileedit.OpUnzip {
			t.Fatalf("%s: status=%d body=%s", name, w.Code, w.Body.String())
		}
	}
}
