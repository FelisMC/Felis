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
	"felis.lolicon.best/internal/worldexport"
)

// Export tickets live in memory; an API restart invalidates them and the associated Jobs fail.

// Exporter is optional; nil yields 503, and Stop deletes abandoned Jobs.
type Exporter interface {
	Start(ctx context.Context, r worldexport.Request) (job string, err error)
	Stop(ctx context.Context, job string) error
}

const (
	exportMaxActive  = 2 // admitted active exports, install-wide
	exportMaxPerUser = 1
	exportPerHour    = 6 // starts per user per hour
	exportCopyBuffer = 32 << 10

	// File downloads have separate limits so small config downloads do not block world exports.
	fileExportMaxActive  = 4
	fileExportMaxPerUser = 2
	fileExportPerHour    = 30
)

type exportClass struct {
	maxActive, perUser, perHour    int
	busyUser, busyActive, busyHour string
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

type exportStarts struct {
	userID string
	files  bool
}

var (
	exportClaimTTL   = 90 * time.Second
	exportPendingTTL = 10 * time.Minute
	exportStall      = 2 * time.Minute
	// exportKeepSpent retains finished tickets as 410 before they become unknown.
	exportKeepSpent = 10 * time.Minute
)

const (
	exportPending   = "pending" // Job has not connected
	exportReady     = "ready"   // upload awaits the browser
	exportStreaming = "streaming"
	exportFailed    = "failed" // Job failed before connecting
	exportSpent     = "spent"  // downloaded or abandoned
)

type exportTicketView struct {
	Ticket   string `json:"ticket"`
	State    string `json:"state"`
	Filename string `json:"filename"`
}

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
	ticket      string
	id          string
	tokenHash   [32]byte
	userID      string
	server      string
	mode        string
	files       bool
	filename    string
	contentType string
	job         string
	state       string
	message     string
	at          time.Time // time of entry into the current state
	upload      *exportUpload
}

type exportUpload struct {
	body io.Reader
	size int64 // declared length; -1 when absent
	// digest is available only after the body ends.
	digest  func() []string
	claimed chan struct{}
	done    chan error // buffered download outcome
}

func (e *exportEntry) active() bool {
	return e.state == exportPending || e.state == exportReady || e.state == exportStreaming
}

type exportRegistry struct {
	mu       sync.Mutex
	byTicket map[string]*exportEntry
	byID     map[string]*exportEntry
	starts   map[exportStarts][]time.Time // oldest first
	// stop deletes expired pending Jobs outside the lock.
	stop func(job string)
}

func (a *API) exportTickets() *exportRegistry {
	a.exportsOnce.Do(func() {
		a.exports = &exportRegistry{
			byTicket: map[string]*exportEntry{},
			byID:     map[string]*exportEntry{},
			starts:   map[exportStarts][]time.Time{},
			stop:     a.stopExportJob,
		}
	})
	return a.exports
}

// ExpireExports sweeps even when no client is polling.
func (a *API) ExpireExports() {
	g := a.exportTickets()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(a.now())
}

// stopExportJob leaves failed deletions to the Job's deadline.
func (a *API) stopExportJob(job string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Exporter.Stop(ctx, job); err != nil {
		log.Printf("api: stop export job %s, which never connected: %v", job, err)
	}
}

// sweepLocked deletes stalled pending Jobs so their world-volume holds cannot block startup until the Job deadline.
func (g *exportRegistry) sweepLocked(now time.Time) {
	for t, e := range g.byTicket {
		switch {
		case e.state == exportPending && now.Sub(e.at) >= exportPendingTTL:
			e.state, e.at = exportSpent, now
			go g.stop(e.job)
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

// drop also refunds the start reservation when Job creation fails.
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

// lookup returns 404-equivalent unknown for another user's ticket and export_expired for a spent ticket.
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

// fail applies only while the Job has not connected.
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

// arrive treats unknown IDs, invalid tokens and non-pending exports identically.
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

// giveUp loses to a concurrent claim and then returns false.
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

// claim spends the ticket even if the subsequent download fails.
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

// handleExportBackup requires former-owner or staff access and reads only the backup store, without a world lock.
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

// handleExportWorld holds the stopped world's volume until download ends to prevent startup during archival.
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

const archiveContentType = "application/gzip"

// handleDownloadFile delegates containment to the Job's os.Root and holds the world volume for the download.
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
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindExport, "wait for the server to stop or finish starting before downloading its files")
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

// exportGate binds the ticket to the caller's account after checking server ownership or staff access.
func (a *API) exportGate(w http.ResponseWriter, r *http.Request) (string, *ServerRecord, bool) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", nil, false
	}
	rec, err := a.managedServerRecord(r.Context(), name)
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

func (a *API) handleExportStatus(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	reg := a.exportTickets()
	e, err := reg.lookup(r.PathValue("ticket"), p.UserID, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Keep the export pending on Job-list failures; polling retries until exportPendingTTL.
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

// handleExportDownload refuses HEAD because claiming spends the ticket before any body bytes are sent.
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

	n, sum, err := relayExport(w, e.upload)
	e.upload.done <- err
	if err != nil {
		log.Printf("api: export %s of %s ended early: %v", e.id, e.server, err)
		// Abort truncated downloads so the browser reports failure instead of accepting an incomplete file.
		panic(http.ErrAbortHandler)
	}
	log.Printf("api: export %s of %s sent %d bytes, sha256 %s", e.id, e.server, n, sum)
}

var (
	errExportDigest = errors.New("the bytes the export Job sent do not match the SHA-256 it sent with them")
	errExportLength = errors.New("the export Job sent a different number of bytes than it declared")
)

// relayExport holds the final buffer until the Job's digest trailer and declared length match, aborting on mismatch.
func relayExport(w http.ResponseWriter, up *exportUpload) (int64, string, error) {
	out := &stallWriter{w: w, rc: http.NewResponseController(w)}
	h := sha256.New()
	var n int64
	next, spare := make([]byte, exportCopyBuffer), make([]byte, exportCopyBuffer)
	var held []byte
	for {
		k, err := up.body.Read(next)
		if k > 0 {
			if len(held) > 0 {
				if _, werr := out.Write(held); werr != nil {
					return n, "", werr
				}
			}
			h.Write(next[:k])
			n += int64(k)
			held = next[:k]
			next, spare = spare, next
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, "", err
		}
	}
	want, err := parseContentDigest(up.digest())
	switch {
	case up.size >= 0 && n != up.size:
		return n, "", fmt.Errorf("%w: %d of %d", errExportLength, n, up.size)
	case err != nil:
		return n, "", fmt.Errorf("%w: %v", errExportDigest, err)
	case subtle.ConstantTimeCompare(h.Sum(nil), want) != 1:
		return n, "", errExportDigest
	}
	if len(held) > 0 {
		if _, err := out.Write(held); err != nil {
			return n, "", err
		}
	}
	_ = out.rc.SetWriteDeadline(time.Time{}) // clear the deadline before connection reuse
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// stallWriter omits ReadFrom so io.CopyBuffer uses the supplied buffer and resets deadlines on each write.
type stallWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (s *stallWriter) Write(p []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(time.Now().Add(exportStall))
	return s.w.Write(p)
}

// handleInternalExportUpload uses a one-time token and waits for the browser; invalid or spent tokens return 404.
func (a *API) handleInternalExportUpload(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeError(w, r, errNoExport())
		return
	}
	size := int64(-1)
	if v := r.Header.Get(worldexport.LengthHeader); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s must be a byte count", worldexport.LengthHeader))
			return
		}
		size = n
	}
	rc := takeBodyDeadline(w, r)
	up := &exportUpload{
		body: &stallBody{r: r.Body, rc: rc}, size: size,
		digest:  func() []string { return r.Trailer.Values(worldexport.DigestTrailer) },
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

// stallBody resets the read deadline for each chunk and clears it when the body ends.
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
