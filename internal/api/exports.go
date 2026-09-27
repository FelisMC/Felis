package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/worldexport"
)

// World export. The owner downloads a tar.gz of their world, either as it is
// now (the server stopped) or as one of its backups, straight into the browser:
//
//  1. POST /servers/{name}/world/export or /servers/{name}/backups/{id}/export
//     checks the caller and the server, admits the export against the limits
//     below and starts a one-shot felis-export Job (internal/worldexport) that
//     reads the world or the archive read-only. It answers 202 with a ticket:
//     256 random bits, good for the caller who started it and nobody else.
//  2. The Job PUTs the archive to the internal face (PUT
//     /api/v1/internal/exports/{id} with the one-time token it was started
//     with), and that request waits, body unread, for the browser.
//  3. The panel polls GET /exports/{ticket} until it reads ready, then points a
//     hidden <a download> at GET /exports/{ticket}/download, which claims the
//     waiting PUT and copies its body into the response 32 KiB at a time. The
//     archive passes through felis-api's memory once and never touches a disk
//     it owns, and the Job moves at the browser's pace.
//
// A backup is checked against the sha256 recorded when it was written as it
// streams, and the last read is held back until the digest is known: a mismatch
// aborts the response, so the browser reports a failed download and never keeps
// a complete-looking corrupt file, and the Job is told backup_corrupt.
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
// buffer alive for as long as a download takes, and a world export also keeps
// its server from starting.
const (
	exportMaxActive  = 2 // admitted and not yet over, install-wide
	exportMaxPerUser = 1
	exportPerHour    = 6 // started by one user in any hour
	exportCopyBuffer = 32 << 10
)

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

var errExportDigest = errors.New("the archive does not match the sha256 recorded when it was written")

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
	sha256    string // what a backup must hash to; empty for a world
	filename  string
	job       string
	state     string
	message   string
	at        time.Time // when it entered its state
	upload    *exportUpload
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
	starts   map[string][]time.Time // per user, oldest first
}

func (a *API) exportTickets() *exportRegistry {
	a.exportsOnce.Do(func() {
		a.exports = &exportRegistry{
			byTicket: map[string]*exportEntry{},
			byID:     map[string]*exportEntry{},
			starts:   map[string][]time.Time{},
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

// admit reserves the export e describes (its user, server, mode, filename and
// digest) and returns its upload token, or refuses it with export_busy.
func (g *exportRegistry) admit(e *exportEntry, now time.Time) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	active, mine := 0, 0
	for _, o := range g.byTicket {
		if o.active() {
			active++
			if o.userID == e.userID {
				mine++
			}
		}
	}
	switch starts := g.starts[e.userID]; {
	case mine >= exportMaxPerUser:
		return "", newError(http.StatusTooManyRequests, "export_busy",
			"you already have an export in progress; download it or let it expire first").retryAfter(exportClaimTTL)
	case active >= exportMaxActive:
		return "", newError(http.StatusTooManyRequests, "export_busy",
			"%d exports are already in progress; retry in a few minutes", exportMaxActive).retryAfter(time.Minute)
	case len(starts) >= exportPerHour:
		return "", newError(http.StatusTooManyRequests, "export_busy",
			"you have started %d exports in the last hour; retry later", exportPerHour).retryAfter(starts[0].Add(time.Hour).Sub(now))
	}
	token := randomHex(32)
	e.ticket, e.id, e.tokenHash = randomHex(32), randomHex(8), sha256.Sum256([]byte(token))
	e.state, e.at = exportPending, now
	g.byTicket[e.ticket] = e
	g.byID[e.id] = e
	g.starts[e.userID] = append(g.starts[e.userID], now)
	return token, nil
}

// drop forgets an export that never got its Job, and gives its start back.
func (g *exportRegistry) drop(e *exportEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byTicket, e.ticket)
	delete(g.byID, e.id)
	ts := g.starts[e.userID]
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(e.at) {
			g.starts[e.userID] = append(ts[:i:i], ts[i+1:]...)
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
		filename: fmt.Sprintf("%s-backup-%s.tar.gz", name, backup.ID), sha256: backup.SHA256}
	token, err := a.exportTickets().admit(e, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !a.startExport(w, r, e, token, backup.BackupRef) {
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
		filename: fmt.Sprintf("%s-world-%s.tar.gz", name, a.now().UTC().Format("20060102-150405"))}
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
	if !a.startExport(w, r, e, token, "") {
		return
	}
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "world.export", ServerName: rec.Name})
	writeJSON(w, http.StatusAccepted, exportTicketView{Ticket: e.ticket, State: exportPending, Filename: e.filename})
}

func errExportUnavailable() error {
	return newError(http.StatusServiceUnavailable, "export_unavailable", "world export is not configured")
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
// writes the error.
func (a *API) startExport(w http.ResponseWriter, r *http.Request, e *exportEntry, token, backupRef string) bool {
	job, err := a.Exporter.Start(r.Context(), worldexport.Request{
		Server: e.server, Mode: e.mode, BackupRef: backupRef, ID: e.id, Token: token,
		TargetURL: a.InternalBaseURL + "/api/v1/internal/exports/" + e.id,
	})
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
	h.Set("Content-Type", "application/gzip")
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

	err = copyExport(w, e.upload.body, e.sha256)
	e.upload.done <- err
	if err != nil {
		log.Printf("api: export %s of %s ended early: %v", e.id, e.server, err)
		// Abort the response rather than end it: a download cut short must
		// read as failed in the browser, never as a complete file.
		panic(http.ErrAbortHandler)
	}
}

// copyExport copies body into w through a fixed 32 KiB buffer, restarting w's
// write deadline on every write. With want set, the last read is held back
// until the whole body hashes to it.
func copyExport(w http.ResponseWriter, body io.Reader, want string) error {
	out := &heldWriter{w: w, rc: http.NewResponseController(w)}
	var sum hash.Hash
	if want != "" {
		sum = sha256.New()
		body = io.TeeReader(body, sum)
	}
	if _, err := io.CopyBuffer(out, body, make([]byte, exportCopyBuffer)); err != nil {
		return err
	}
	if sum != nil && !strings.EqualFold(hex.EncodeToString(sum.Sum(nil)), want) {
		return errExportDigest
	}
	if err := out.flush(); err != nil {
		return err
	}
	_ = out.rc.SetWriteDeadline(time.Time{}) // the connection may serve another request
	return nil
}

// heldWriter passes each write on one behind, keeping the latest back until
// flush, so the end of an archive reaches the browser only once it is checked.
// It has no ReadFrom, so io.CopyBuffer uses the buffer it is given.
type heldWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	held []byte
}

func (h *heldWriter) Write(p []byte) (int, error) {
	if err := h.flush(); err != nil {
		return 0, err
	}
	h.held = append(h.held[:0], p...)
	return len(p), nil
}

func (h *heldWriter) flush() error {
	if len(h.held) == 0 {
		return nil
	}
	_ = h.rc.SetWriteDeadline(time.Now().Add(exportStall))
	_, err := h.w.Write(h.held)
	h.held = h.held[:0]
	return err
}

// handleInternalExportUpload takes an export Job's archive (PUT
// /api/v1/internal/exports/{id}). The one-time bearer token minted with the
// export is the check, as for file uploads: an unknown id, a wrong token and a
// token already used all read the same 404. The request then waits for the
// browser, up to exportClaimTTL, and answers once the download has ended: 204
// when it got the whole archive, 409 backup_corrupt when the archive did not
// match its recorded digest, 410 export_expired when nobody came for it or the
// browser left early.
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
	switch err := <-up.done; {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errExportDigest):
		writeError(w, r, newError(http.StatusConflict, "backup_corrupt", "%v", err))
	default:
		writeError(w, r, newError(http.StatusGone, "export_expired", "the download ended before the archive did: %v", err))
	}
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
