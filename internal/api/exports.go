package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/worldexport"
)

// World export and file download. The owner downloads a tar.gz of their world,
// either as it is now (the server stopped) or as one of its backups, or one file
// or folder of a stopped world (a folder as a zip), straight into the browser:
//
//  1. POST /servers/{name}/world/export, /servers/{name}/backups/{id}/export or
//     /servers/{name}/files/download checks the caller and the server, admits
//     the export against the limits below and starts a one-shot felis-export
//     Job (internal/worldexport) that reads the world or the archive read-only.
//     It answers 202 with a ticket: 256 random bits, good for the caller who
//     started it and nobody else.
//  2. The Job PUTs the archive to the internal face (PUT
//     /api/v1/internal/exports/{id} with the one-time token it was started
//     with), and that request waits, body unread, for the browser.
//  3. The panel polls GET /exports/{ticket} until it reads ready, then points a
//     hidden <a download> at GET /exports/{ticket}/download, which claims the
//     waiting PUT and copies its body into the response 32 KiB at a time. The
//     archive passes through felis-api's memory once and never touches a disk
//     it owns, and the Job moves at the browser's pace.
//
// A backup is checked by the Job against the sha256 recorded when it was
// written (cmd/felis export): on a mismatch the Job aborts its upload short of
// the archive's end, the download aborts with it, and the browser reports a
// failed download and never keeps a complete-looking corrupt file.
//
// Tickets live in felis-api's memory. A restart forgets them, and a Job that
// then PUTs finds nothing and fails, which the jobs list shows.

// Exporter starts the Job that archives a world or a backup and hands it to the
// internal upload route (internal/worldexport). Optional: when nil the export
// routes answer 503.
type Exporter interface {
	Start(ctx context.Context, r worldexport.Request) (job string, err error)
}

// Limits on exports. Each one keeps a Job, a connection and a 64 KiB copy
// buffer alive for as long as a download takes, and a world export or a file
// download also keeps its server from starting.
const (
	exportMaxActive  = 2 // admitted and not yet over, install-wide
	exportMaxPerUser = 1
	exportPerHour    = 6 // started by one user in any hour
	exportCopyBuffer = 32 << 10

	// File downloads count apart from the world and backup exports, with
	// their own limits: one is a file or a folder, usually small and over in
	// seconds, and an owner fetching a few configs one after another must
	// neither wait on an export nor hold one off.
	fileExportMaxActive  = 4
	fileExportMaxPerUser = 2
	fileExportPerHour    = 30
)

// exportClass is one set of export limits and how a refusal names them.
type exportClass struct {
	maxActive, perUser, perHour    int
	busyUser, busyActive, busyHour string // each formats its limit
}

var (
	worldExports = exportClass{
		maxActive: exportMaxActive, perUser: exportMaxPerUser, perHour: exportPerHour,
		busyUser:   "you already have %d export in progress; download it or let it expire first",
		busyActive: "%d exports are already in progress; retry in a few minutes",
		busyHour:   "you have started %d exports in the last hour; retry later",
	}
	fileExports = exportClass{
		maxActive: fileExportMaxActive, perUser: fileExportMaxPerUser, perHour: fileExportPerHour,
		busyUser:   "you already have %d file downloads in progress; let one finish first",
		busyActive: "%d file downloads are already in progress; retry in a minute",
		busyHour:   "you have started %d file downloads in the last hour; retry later",
	}
)

func (e *exportEntry) class() exportClass {
	if e.files {
		return fileExports
	}
	return worldExports
}

// exportStarts keys a user's recent starts within one class.
type exportStarts struct {
	userID string
	files  bool
}

// Timings. Vars only so a test can shrink them.
var (
	// exportClaimTTL is how long the Job's upload waits for the browser.
	exportClaimTTL = 90 * time.Second
	// exportPendingTTL is how long an export may take to reach ready: the Job
	// is scheduled, pulls its image and connects well inside it.
	exportPendingTTL = 10 * time.Minute
	// exportStall is the longest either end may go without moving a byte.
	exportStall = 2 * time.Minute
	// exportKeepSpent is how long a finished ticket still answers 410
	// export_expired before it reads as unknown.
	exportKeepSpent = 10 * time.Minute
)

// States of an export.
const (
	exportPending   = "pending"   // the Job has not connected yet
	exportReady     = "ready"     // its upload is waiting for the browser
	exportStreaming = "streaming" // the browser is downloading
	exportFailed    = "failed"    // the Job died before it connected
	exportSpent     = "spent"     // downloaded, or given up
)

// exportTicketView answers both export routes.
type exportTicketView struct {
	Ticket   string `json:"ticket"`
	State    string `json:"state"`
	Filename string `json:"filename"`
}

// exportStatusView answers GET /exports/{ticket}.
type exportStatusView struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

func errExportExpired() error {
	return newError(http.StatusGone, "export_expired", "this export has expired or was already downloaded; start a new one")
}

func errNoExport() error {
	return newError(http.StatusNotFound, "not_found", "no such export")
}

type exportEntry struct {
	ticket    string
	id        string
	tokenHash [32]byte
	userID    string
	server    string
	mode      string
	files     bool // a file download, counted in fileExports
	filename  string
	// contentType is what the download is served as: a gzip archive, a zip,
	// or a single file's raw bytes.
	contentType string
	job         string
	state       string
	message     string
	at          time.Time // when it entered its state
	upload      *exportUpload
}

// exportUpload is the Job's PUT, parked until a browser claims it.
type exportUpload struct {
	body    io.Reader
	size    int64 // -1 when the Job streams it chunked
	claimed chan struct{}
	done    chan error // how the download ended; buffered
}

func (e *exportEntry) active() bool {
	return e.state == exportPending || e.state == exportReady || e.state == exportStreaming
}

type exportRegistry struct {
	mu       sync.Mutex
	byTicket map[string]*exportEntry
	byID     map[string]*exportEntry
	starts   map[exportStarts][]time.Time // oldest first
}

func (a *API) exportTickets() *exportRegistry {
	a.exportsOnce.Do(func() {
		a.exports = &exportRegistry{
			byTicket: map[string]*exportEntry{},
			byID:     map[string]*exportEntry{},
			starts:   map[exportStarts][]time.Time{},
		}
	})
	return a.exports
}

// sweepLocked expires what has waited too long and forgets what ended long ago.
func (g *exportRegistry) sweepLocked(now time.Time) {
	for t, e := range g.byTicket {
		switch {
		case e.state == exportPending && now.Sub(e.at) >= exportPendingTTL:
			e.state, e.at = exportSpent, now
		case !e.active() && now.Sub(e.at) >= exportKeepSpent:
			delete(g.byTicket, t)
			delete(g.byID, e.id)
		}
	}
	for u, ts := range g.starts {
		for len(ts) > 0 && now.Sub(ts[0]) >= time.Hour {
			ts = ts[1:]
		}
		if len(ts) == 0 {
			delete(g.starts, u)
		} else {
			g.starts[u] = ts
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// admit reserves the export e describes (its user, server, mode, class,
// filename and content type) and returns its upload token, or refuses it with
// export_busy. Only exports of e's class count against it.
func (g *exportRegistry) admit(e *exportEntry, now time.Time) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	active, mine := 0, 0
	for _, o := range g.byTicket {
		if o.active() && o.files == e.files {
			active++
			if o.userID == e.userID {
				mine++
			}
		}
	}
	c, key := e.class(), exportStarts{e.userID, e.files}
	switch starts := g.starts[key]; {
	case mine >= c.perUser:
		return "", newError(http.StatusTooManyRequests, "export_busy", c.busyUser, c.perUser).retryAfter(exportClaimTTL)
	case active >= c.maxActive:
		return "", newError(http.StatusTooManyRequests, "export_busy", c.busyActive, c.maxActive).retryAfter(time.Minute)
	case len(starts) >= c.perHour:
		return "", newError(http.StatusTooManyRequests, "export_busy", c.busyHour, c.perHour).retryAfter(starts[0].Add(time.Hour).Sub(now))
	}
	token := randomHex(32)
	e.ticket, e.id, e.tokenHash = randomHex(32), randomHex(8), sha256.Sum256([]byte(token))
	e.state, e.at = exportPending, now
	g.byTicket[e.ticket] = e
	g.byID[e.id] = e
	g.starts[key] = append(g.starts[key], now)
	return token, nil
}

// drop forgets an export that never got its Job, and gives its start back.
func (g *exportRegistry) drop(e *exportEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byTicket, e.ticket)
	delete(g.byID, e.id)
	key := exportStarts{e.userID, e.files}
	ts := g.starts[key]
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(e.at) {
			if rest := append(ts[:i:i], ts[i+1:]...); len(rest) > 0 {
				g.starts[key] = rest
			} else {
				delete(g.starts, key)
			}
			break
		}
	}
}

func (g *exportRegistry) started(e *exportEntry, job string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e.job = job
}

// lookup returns a copy of userID's export by its ticket. Another user's reads
// as unknown, and a spent one as export_expired.
func (g *exportRegistry) lookup(ticket, userID string, now time.Time) (exportEntry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	e, ok := g.byTicket[ticket]
	switch {
	case !ok || e.userID != userID:
		return exportEntry{}, errNoExport()
	case e.state == exportSpent || e.state == exportStreaming:
		return exportEntry{}, errExportExpired()
	}
	return *e, nil
}

// fail records that the export's Job died before it connected, and reports
// false when the export has moved on since.
func (g *exportRegistry) fail(ticket, message string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.byTicket[ticket]
	if !ok || e.state != exportPending {
		return false
	}
	e.state, e.message, e.at = exportFailed, message, now
	return true
}

// arrive parks the Job's upload on the export the id and token open. Unknown
// ids, wrong tokens and exports past pending all read the same.
func (g *exportRegistry) arrive(id, token string, up *exportUpload, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	e, ok := g.byID[id]
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(sum[:], e.tokenHash[:]) != 1 || e.state != exportPending {
		return false
	}
	e.state, e.at, e.upload = exportReady, now, up
	return true
}

// giveUp spends an export whose upload nobody claimed, and reports false when a
// claim got there first.
func (g *exportRegistry) giveUp(up *exportUpload, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.byID {
		if e.upload == up && e.state == exportReady {
			e.state, e.at = exportSpent, now
			return true
		}
	}
	return false
}

// claim hands userID's ready export to its download: from here on the ticket is
// spent whatever happens to the download.
func (g *exportRegistry) claim(ticket, userID string, now time.Time) (exportEntry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	e, ok := g.byTicket[ticket]
	switch {
	case !ok || e.userID != userID:
		return exportEntry{}, errNoExport()
	case e.state == exportPending:
		return exportEntry{}, newError(http.StatusConflict, "export_not_ready",
			"the export is still being prepared; wait until its status reads ready")
	case e.state != exportReady:
		return exportEntry{}, errExportExpired()
	}
	e.state, e.at = exportStreaming, now
	close(e.upload.claimed)
	return *e, nil
}

func (g *exportRegistry) finish(ticket string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.byTicket[ticket]; ok {
		e.state, e.at = exportSpent, now
	}
}

// handleExportBackup starts the download of one backup (POST
// /api/v1/servers/{name}/backups/{id}/export). The gate is restore's and
// delete's together:
//
//	① name validation; ② an unknown server is 404; ③ owner-or-admin, else 403
//	④ the backup must exist, and a non-admin must be its former owner: any
//	   other id reads as unknown (404 no_backup), as it does in their list
//	⑤ cross-server guard: the backup must be this server's (403)
//	⑥ an archive that failed its read-back is 409 backup_corrupt
//
// It needs no stopped server and takes no world lock: the Job reads the backup
// store only.
func (a *API) handleExportBackup(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name, rec, ok := a.exportGate(w, r)
	if !ok {
		return
	}
	backup, err := a.Repo.BackupByID(r.Context(), r.PathValue("id"))
	if err == nil && !p.IsAdmin() && backup.FormerOwner != p.UserID {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, r, newError(http.StatusNotFound, "no_backup", "no matching backup exists"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if backup.ServerName != name {
		writeError(w, r, errForbidden)
		return
	}
	if backup.Corrupt {
		writeError(w, r, newError(http.StatusConflict, "backup_corrupt",
			"this backup did not read back intact and cannot be exported; pick another"))
		return
	}
	if a.Exporter == nil || a.InternalBaseURL == "" {
		writeError(w, r, errExportUnavailable())
		return
	}
	e := &exportEntry{userID: p.UserID, server: name, mode: worldexport.ModeBackup,
		filename: fmt.Sprintf("%s-backup-%s.tar.gz", name, backup.ID), contentType: archiveContentType}
	token, err := a.exportTickets().admit(e, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !a.startExport(w, r, e, token, worldexport.Request{BackupRef: backup.BackupRef, BackupSHA256: backup.SHA256}) {
		return
	}
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "backup.export", ServerName: rec.Name,
		Payload: auditPayload(map[string]any{"backup_id": backup.ID, "size_bytes": backup.SizeBytes})})
	writeJSON(w, http.StatusAccepted, exportTicketView{Ticket: e.ticket, State: exportPending, Filename: e.filename})
}

// handleExportWorld starts the download of a stopped server's world as it is
// now (POST /api/v1/servers/{name}/world/export). The gate is a backup's: owner
// or admin, the server fully stopped, a world volume to read, and the
// world-volume lock, which the export Job then holds (as KindExport) until the
// download ends, so the server cannot start under it and tear the archive.
func (a *API) handleExportWorld(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name, rec, ok := a.exportGate(w, r)
	if !ok {
		return
	}
	const notStopped = "stop the server before exporting its world"
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) {
		writeError(w, r, newError(http.StatusConflict, "not_stopped", notStopped))
		return
	}
	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	} else if !exists {
		writeError(w, r, errNoWorldVolume())
		return
	}
	if a.Exporter == nil || a.InternalBaseURL == "" {
		writeError(w, r, errExportUnavailable())
		return
	}
	reg := a.exportTickets()
	e := &exportEntry{userID: p.UserID, server: name, mode: worldexport.ModeWorld,
		filename:    fmt.Sprintf("%s-world-%s.tar.gz", name, a.now().UTC().Format("20060102-150405")),
		contentType: archiveContentType}
	token, err := reg.admit(e, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindExport, notStopped)
	if !ok {
		reg.drop(e)
		return
	}
	defer release()
	if !a.startExport(w, r, e, token, worldexport.Request{}) {
		return
	}
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "world.export", ServerName: rec.Name})
	writeJSON(w, http.StatusAccepted, exportTicketView{Ticket: e.ticket, State: exportPending, Filename: e.filename})
}

// archiveContentType is how a world or a backup export is served.
const archiveContentType = "application/gzip"

// handleDownloadFile starts the download of one file or folder of a stopped
// server's world (POST /api/v1/servers/{name}/files/download?path=…&dir=true
// for a folder). The gate is the file manager's (authorizeFileOp), plus an
// account to bind the ticket to. The download is an export: a felis-export Job
// in files mode reads the file, or zips the folder, from the world volume and
// hands it over through a ticket like any other, under the world-volume lock
// (as KindExport) so the server cannot start mid-zip.
//
// The path is passed to the Job as it came, as every file route does (see
// handleListFiles): the Job's os.Root is the containment, and the guards run
// there. Only the world root is refused here, since a whole world is what the
// world export is for.
func (a *API) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	p := principalFromContext(r.Context())
	if p.UserID == "" {
		writeError(w, r, errForbidden)
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("dir") == "true"
	base := pathpkg.Base(pathpkg.Clean("/" + path))
	if base == "/" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_path",
			"the whole world is not a file download; export the world from the backups page instead"))
		return
	}
	if a.Exporter == nil || a.InternalBaseURL == "" {
		writeError(w, r, errExportUnavailable())
		return
	}
	e := &exportEntry{userID: p.UserID, server: name, mode: worldexport.ModeFiles, files: true,
		filename: base, contentType: fileedit.DownloadFileType}
	if dir {
		e.filename, e.contentType = base+".zip", fileedit.DownloadZipType
	}
	reg := a.exportTickets()
	token, err := reg.admit(e, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindExport, "stop the server before editing its files")
	if !ok {
		reg.drop(e)
		return
	}
	defer release()
	if !a.startExport(w, r, e, token, worldexport.Request{Path: path, Dir: dir}) {
		return
	}
	a.auditFile(r, "file.download", name, path, map[string]any{"dir": dir})
	writeJSON(w, http.StatusAccepted, exportTicketView{Ticket: e.ticket, State: exportPending, Filename: e.filename})
}

func errExportUnavailable() error {
	return newError(http.StatusServiceUnavailable, "export_unavailable", "world export and downloads are not configured")
}

// exportGate is the front half both export routes share: a valid name, a known
// server, and a caller who owns it or is an admin. The ticket is bound to the
// caller's account, so a principal without one cannot start an export.
func (a *API) exportGate(w http.ResponseWriter, r *http.Request) (string, *ServerRecord, bool) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", nil, false
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", nil, false
	}
	if p.UserID == "" || !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return "", nil, false
	}
	return name, rec, true
}

// startExport creates the admitted export's Job, or forgets the export and
// writes the error. what carries the mode's own fields (the backup and its
// digest, or the path); the rest comes from e.
func (a *API) startExport(w http.ResponseWriter, r *http.Request, e *exportEntry, token string, what worldexport.Request) bool {
	what.Server, what.Mode, what.ID, what.Token = e.server, e.mode, e.id, token
	what.TargetURL = a.InternalBaseURL + "/api/v1/internal/exports/" + e.id
	job, err := a.Exporter.Start(r.Context(), what)
	if err != nil {
		a.exportTickets().drop(e)
		writeError(w, r, err)
		return false
	}
	a.exportTickets().started(e, job)
	return true
}

// handleExportStatus answers GET /api/v1/exports/{ticket}: pending while the
// Job gets going, ready once its upload waits for the browser, failed (with the
// Job's error) when it died first. A downloaded or expired ticket is 410
// export_expired; an unknown one, or another user's, is 404.
func (a *API) handleExportStatus(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	reg := a.exportTickets()
	e, err := reg.lookup(r.PathValue("ticket"), p.UserID, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	// A Job list that fails leaves the export pending: the next poll asks again,
	// and exportPendingTTL ends it either way.
	if e.state == exportPending && e.job != "" && a.JobStatus != nil {
		jobs, _ := a.JobStatus.LatestJobs(r.Context(), e.server)
		for _, j := range jobs {
			if j.Name == e.job && j.State == "failed" && reg.fail(e.ticket, j.Message, a.now()) {
				e.state, e.message = exportFailed, j.Message
			}
		}
	}
	writeJSON(w, http.StatusOK, exportStatusView{State: e.state, Message: e.message})
}

// handleExportDownload streams a ready export to its browser (GET
// /api/v1/exports/{ticket}/download). The claim spends the ticket before the
// first byte moves, so a second tab, a retry or a HEAD probe never gets a
// second copy; a HEAD is refused outright, since claiming on it would spend the
// ticket on a response with no body.
func (a *API) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, newError(http.StatusMethodNotAllowed, "method_not_allowed",
			"%s is not allowed here; use GET", r.Method))
		return
	}
	p := principalFromContext(r.Context())
	reg := a.exportTickets()
	e, err := reg.claim(r.PathValue("ticket"), p.UserID, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer reg.finish(e.ticket, a.now())

	h := w.Header()
	h.Set("Content-Type", e.contentType)
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": e.filename}); cd != "" {
		h.Set("Content-Disposition", cd)
	} else {
		h.Set("Content-Disposition", "attachment")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if e.upload.size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(e.upload.size, 10))
	}
	w.WriteHeader(http.StatusOK)

	err = copyExport(w, e.upload.body)
	e.upload.done <- err
	if err != nil {
		log.Printf("api: export %s of %s ended early: %v", e.id, e.server, err)
		// Abort the response rather than end it: a download cut short must
		// read as failed in the browser, never as a complete file.
		panic(http.ErrAbortHandler)
	}
}

// copyExport copies body into w through a fixed 32 KiB buffer, restarting w's
// write deadline on every write.
func copyExport(w http.ResponseWriter, body io.Reader) error {
	out := &stallWriter{w: w, rc: http.NewResponseController(w)}
	if _, err := io.CopyBuffer(out, body, make([]byte, exportCopyBuffer)); err != nil {
		return err
	}
	_ = out.rc.SetWriteDeadline(time.Time{}) // the connection may serve another request
	return nil
}

// stallWriter restarts the connection's write deadline before every write, so
// a write fails only once the browser has taken nothing for exportStall. It
// has no ReadFrom, so io.CopyBuffer uses the buffer it is given.
type stallWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (s *stallWriter) Write(p []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(time.Now().Add(exportStall))
	return s.w.Write(p)
}

// handleInternalExportUpload takes an export Job's archive (PUT
// /api/v1/internal/exports/{id}). The one-time bearer token minted with the
// export is the check, as for file uploads: an unknown id, a wrong token and a
// token already used all read the same 404. The request then waits for the
// browser, up to exportClaimTTL, and answers once the download has ended: 204
// when it got the whole archive, 410 export_expired when nobody came for it,
// the browser left early or the Job itself cut the upload short.
func (a *API) handleInternalExportUpload(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeError(w, r, errNoExport())
		return
	}
	rc := takeBodyDeadline(w, r)
	up := &exportUpload{
		body: &stallBody{r: r.Body, rc: rc}, size: r.ContentLength,
		claimed: make(chan struct{}), done: make(chan error, 1),
	}
	reg := a.exportTickets()
	if !reg.arrive(r.PathValue("id"), token, up, a.now()) {
		writeError(w, r, errNoExport())
		return
	}
	timer := time.NewTimer(exportClaimTTL)
	defer timer.Stop()
	select {
	case <-up.claimed:
	case <-timer.C:
		if reg.giveUp(up, a.now()) {
			writeError(w, r, errExportExpired())
			return
		}
	case <-r.Context().Done():
		if reg.giveUp(up, a.now()) {
			return
		}
	}
	if err := <-up.done; err != nil {
		writeError(w, r, newError(http.StatusGone, "export_expired", "the download ended before the archive did: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stallBody restarts the connection's read deadline on every read, so reading
// fails only once no byte has come for exportStall, and clears it when the body
// ends, as deadlineBody does.
type stallBody struct {
	r    io.Reader
	rc   *http.ResponseController
	done bool
}

func (b *stallBody) Read(p []byte) (int, error) {
	if b.done {
		return b.r.Read(p)
	}
	_ = b.rc.SetReadDeadline(time.Now().Add(exportStall))
	n, err := b.r.Read(p)
	if err != nil {
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}
