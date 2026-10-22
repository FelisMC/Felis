package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/netguard"
)

func (a *API) handleSearchFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, query := r.URL.Query().Get("path"), strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" || len(query) > 256 || len(path) > 4096 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "q must contain 1 to 256 bytes and path at most 4096 bytes"))
		return
	}
	ls, err := a.Files.Search(r.Context(), name, path, query)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	if ls.Entries == nil {
		ls.Entries = []fileedit.Entry{}
	}
	var free any = ls.Free
	if ls.Free < 0 {
		free = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": ls.Entries, "truncated": ls.Truncated, "free_bytes": free})
}

type transferFilesRequest struct {
	Op        string   `json:"op"`
	Paths     []string `json:"paths"`
	To        *string  `json:"to"`
	Overwrite bool     `json:"overwrite,omitempty"`
}

type zipFilesRequest struct {
	Paths     []string `json:"paths"`
	To        string   `json:"to"`
	Overwrite bool     `json:"overwrite,omitempty"`
}

func (a *API) handleTransferFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	var body transferFilesRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Op != "copy" && body.Op != "move" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "op must be copy or move"))
		return
	}
	if err := selectedFilePaths(body.Paths); err != nil {
		writeError(w, r, err)
		return
	}
	if body.To == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "the to field is required"))
		return
	}
	if err := fileTargetPath(*body.To, true); err != nil {
		writeError(w, r, err)
		return
	}
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to finish starting or stopping before editing its files")
	if !ok {
		return
	}
	defer release()
	op, err := a.Files.StartTransfer(r.Context(), name, body.Op, body.Paths, *body.To, body.Overwrite)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file."+body.Op, name, *body.To, map[string]any{"paths": body.Paths, "overwrite": body.Overwrite})
	writeJSON(w, http.StatusAccepted, map[string]any{"op": opView(op)})
}

func (a *API) handleZipFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	var body zipFilesRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := selectedFilePaths(body.Paths); err != nil {
		writeError(w, r, err)
		return
	}
	if err := fileTargetPath(body.To, false); err != nil {
		writeError(w, r, err)
		return
	}
	if !strings.HasSuffix(strings.ToLower(body.To), ".zip") {
		writeError(w, r, newError(http.StatusBadRequest, "bad_path", "to must name a .zip file"))
		return
	}
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to finish starting or stopping before editing its files")
	if !ok {
		return
	}
	defer release()
	op, err := a.Files.StartZip(r.Context(), name, body.Paths, body.To, body.Overwrite)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.zip", name, body.To, map[string]any{"paths": body.Paths, "overwrite": body.Overwrite})
	writeJSON(w, http.StatusAccepted, map[string]any{"op": opView(op)})
}

func selectedFilePaths(paths []string) error {
	if len(paths) == 0 || len(paths) > fileedit.MaxSelectedPaths {
		return newError(http.StatusBadRequest, "bad_request", "select 1 to %d paths", fileedit.MaxSelectedPaths)
	}
	total := 0
	for _, path := range paths {
		if err := fileTargetPath(path, false); err != nil {
			return err
		}
		total += len(path)
	}
	if total > fileedit.MaxSelectedPathBytes {
		return newError(http.StatusBadRequest, "bad_request", "selected paths exceed %d bytes", fileedit.MaxSelectedPathBytes)
	}
	return nil
}

func fileTargetPath(path string, root bool) error {
	if root && (path == "" || path == ".") {
		return nil
	}
	if len(path) > 4096 || !filepath.IsLocal(path) || filepath.Clean(path) == "." || strings.ContainsRune(path, 0) {
		return newError(http.StatusBadRequest, "bad_path", "the path must name an entry inside the server folder")
	}
	if !namesFit(path) {
		return errNameTooLong()
	}
	return nil
}

func extractableArchive(path string) bool {
	path = strings.ToLower(path)
	for _, ext := range []string{".zip", ".tar", ".tar.gz", ".tgz"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

type fileFromURLRequest struct {
	URL       string `json:"url"`
	Path      string `json:"path"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

func (a *API) handleFileFromURL(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	user, ok := a.requireFileStage(w, r)
	if !ok {
		return
	}
	var body fileFromURLRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := fileTargetPath(body.Path, false); err != nil {
		writeError(w, r, err)
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || len(body.URL) > 8192 || publicFileURL(u) != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_url", "url must be an HTTP or HTTPS URL on a public host without credentials"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_url", "url is invalid"))
		return
	}
	client := newFileDownloadClient()
	if a.FileDownloadClient != nil {
		client = *a.FileDownloadClient
	}
	client.CheckRedirect = checkFileRedirect
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errFileURL) || errors.Is(err, netguard.ErrNotPublic) {
			writeError(w, r, newError(http.StatusBadRequest, "bad_url", "the source redirected or resolved to a destination that is not public"))
			return
		}
		writeError(w, r, newError(http.StatusBadGateway, "download_failed", "the source could not be downloaded"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, r, newError(http.StatusBadGateway, "download_failed", "the source returned HTTP %d", resp.StatusCode))
		return
	}
	staged, drop, err := a.FileStage.Receive(user, name, body.Path, resp.Body, resp.ContentLength)
	if err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	started := false
	defer func() {
		if !started {
			drop()
		}
	}()
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to finish starting or stopping before editing its files")
	if !ok {
		return
	}
	defer release()
	op, err := a.Files.StartUpload(r.Context(), name, body.Path, fileedit.UploadSource{
		URL:   a.InternalBaseURL + "/api/v1/internal/file-uploads/" + staged.ID,
		Token: staged.Token, Size: staged.Size, SHA256: staged.SHA256,
	}, body.Overwrite)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	started = true
	a.auditFile(r, "file.from_url", name, body.Path, map[string]any{
		"url": body.URL, "size_bytes": staged.Size, "sha256": staged.SHA256, "overwrite": body.Overwrite,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"op": opView(op)})
}

func newFileDownloadClient() http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.RefusePrivate}
	return http.Client{Transport: &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout: 30 * time.Second, DisableKeepAlives: true,
	}}
}

var errFileURL = errors.New("invalid public download URL")

func checkFileRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errFileURL
	}
	return publicFileURL(req.URL)
}

func publicFileURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return errFileURL
	}
	host := u.Hostname()
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return errFileURL
	}
	port := u.Port()
	if port == "" {
		port = "80"
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errFileURL
	}
	if net.ParseIP(host) != nil || strings.Contains(host, ":") {
		return netguard.RefusePrivate("tcp", net.JoinHostPort(host, port), nil)
	}
	return nil
}
