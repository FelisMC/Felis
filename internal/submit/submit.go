// Package submit requires staff approval before building user modpacks through the normal isolated, scan-gated build pipeline.
package submit

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/build"
)

type Status string

const (
	StatusPendingReview Status = "pending_review"
	StatusApproved      Status = "approved"
	StatusRejected      Status = "rejected"
)

var (
	ErrInvalid            = errors.New("submit: invalid request")
	ErrNotFound           = errors.New("submit: submission not found")
	ErrAlreadyReviewed    = errors.New("submit: submission already reviewed")
	ErrQuotaExceeded      = errors.New("submit: quota exceeded")
	ErrUploadsUnavailable = errors.New("submit: context upload transport not configured")
	// ErrContextChanged refuses approval after a re-upload changed the reviewed digest.
	ErrContextChanged = errors.New("submit: the build context changed since it was reviewed")
	ErrBlobNotFound   = errors.New("submit: context blob not found")
	// ErrStoreUnavailable means budget checks failed before writing; the upload can retry.
	ErrStoreUnavailable = errors.New("submit: the uploads store did not answer")
)

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

var errContextTooLarge = fmt.Errorf("%w: build context exceeds the maximum allowed size", ErrInvalid)

var errStorageQuota = fmt.Errorf("%w: the upload exceeds your remaining storage allowance", ErrQuotaExceeded)

var ErrUploadsFull = errors.New("the uploads store is full")

const (
	maxDisplayName           = 200
	maxRejectReason          = 1000
	defaultMaxContextBytes   = 1 << 30
	defaultMaxPendingPerUser = 5
	// Per-user storage counts unapproved contexts; approved contexts count only toward the shared store budget.
	defaultMaxStoredBytesPerUser = 2 << 30
	// The total-byte cap also protects node disks where local-path PVC capacity is not enforced.
	defaultMaxStoredBytesTotal = 4 << 30
)

// RejectedContextRetention expires rejected blobs while preserving rows; approved contexts remain available for registry rebuilds.
const RejectedContextRetention = 7 * 24 * time.Hour

// RoomChecker reserves enough filesystem headroom for the worst-case upload, returning ErrUploadsFull when it would be exhausted.
type RoomChecker interface {
	CheckRoom(need int64) error
}

var displayNameRE = regexp.MustCompile(`^[\p{L}\p{N} ._:()\[\]/+-]+$`)

// Submission accepts only DisplayName from the user; context, image, build and reviewer fields are platform-controlled.
type Submission struct {
	ID           string     `json:"id"`
	SubmittedBy  string     `json:"submitted_by"`
	DisplayName  string     `json:"display_name"`
	ContextRef   string     `json:"context_ref"`
	Status       Status     `json:"status"`
	ImageRef     string     `json:"image_ref,omitempty"`
	BuildID      string     `json:"build_id,omitempty"`
	ReviewedBy   string     `json:"reviewed_by,omitempty"`
	RejectReason string     `json:"reject_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`

	// ContextSHA256 is empty before upload; approval and fetching must bind to its recorded digest.
	ContextSHA256 string `json:"context_sha256,omitempty"`
}

// ListOpts.SubmittedBy empty selects all users; Status is exact and Query is case-insensitive across ID, submitter and label.
type ListOpts struct {
	SubmittedBy string
	Status      Status
	Query       string
	Limit       int
	Offset      int
}

// Page.Counts ignores Status and Query filters while Total reflects them; both remain scoped to SubmittedBy.
type Page struct {
	Submissions []Submission
	Total       int
	Counts      map[Status]int
}

const (
	DefaultListLimit = 20
	MaxListLimit     = 100
)

type Store interface {
	// CreateSubmission atomically checks maxPending and inserts, returning the previous count or ErrQuotaExceeded.
	CreateSubmission(ctx context.Context, s *Submission, maxPending int) (int, error)
	GetSubmission(ctx context.Context, id string) (*Submission, error)
	// ListSubmissions is a full scan for budgets and retention; user-facing lists must use PageSubmissions.
	ListSubmissions(ctx context.Context) ([]Submission, error)
	PageSubmissions(ctx context.Context, opts ListOpts) (Page, error)
	// ApproveSubmission compare-and-sets pending status and context digest; only the winner may start a build.
	ApproveSubmission(ctx context.Context, id, reviewedBy, imageRef, digest string, at time.Time) (won bool, err error)
	// SetContextDigest returns false if the row stopped being pending.
	SetContextDigest(ctx context.Context, id, digest string) (bool, error)
	// RejectSubmission atomically records the verdict and reports whether this call won.
	RejectSubmission(ctx context.Context, id, reviewedBy, reason string, at time.Time) (won bool, err error)
	// LinkBuild runs after Submit; a missing build ID can still mean a build is running if linkage failed.
	LinkBuild(ctx context.Context, id, buildID string) error
	DeleteSubmission(ctx context.Context, id string) (bool, error)
	// DeletePendingSubmission atomically checks submitter and pending status so review cannot lose its build context.
	DeletePendingSubmission(ctx context.Context, id, submittedBy string) (bool, error)
}

type Builds interface {
	Submit(ctx context.Context, req build.Request) (*build.Build, error)
}

// Blobs keys are submission IDs, never caller paths; Put must atomically replace a complete context.
type Blobs interface {
	Put(ctx context.Context, id string, r io.Reader) (int64, error)
	Exists(ctx context.Context, id string) (bool, error)
	// Size must read actual blob bytes rather than cached database sizes; ok=false means absent.
	Size(ctx context.Context, id string) (int64, bool, error)
	// Delete is idempotent and runs after row removal so cleanup cannot invalidate a live submission.
	Delete(ctx context.Context, id string) error
	Open(ctx context.Context, id string) (io.ReadCloser, error)
}

// Manager caches blob sizes; use it by pointer.
type Manager struct {
	Store  Store
	Builds Builds

	// Registry must match build.Config.RegistryURL to prevent prevalidation from passing a target that Submit rejects.
	Registry     string
	ContextStore string
	// ContextBaseURL serves blobs over the internal API because build Jobs cannot mount the uploads PVC or carry store credentials.
	ContextBaseURL string
	// Blobs must back ContextStore; nil disables uploads with ErrUploadsUnavailable.
	Blobs                 Blobs
	MaxContextBytes       int64
	MaxPendingPerUser     int
	MaxStoredBytesPerUser int64
	MaxStoredBytesTotal   int64
	// Parts=nil disables chunked uploads while retaining single-request uploads.
	Parts        *PartStore
	PartMaxBytes int64

	Now   func() time.Time
	IDGen func() string

	// sizes caches blob sizes to avoid scanning the store before every upload part.
	sizesMu sync.Mutex
	sizes   map[string]knownSize
}

type knownSize struct {
	n  int64
	at time.Time
}

// blobSizeTTL bounds cross-replica cache staleness; final UploadContext checks always read fresh store sizes.
const blobSizeTTL = time.Minute

func (m *Manager) ContextLimit() int64 { return m.maxContextBytes() }

func (m *Manager) maxContextBytes() int64 {
	if m.MaxContextBytes > 0 {
		return m.MaxContextBytes
	}
	return defaultMaxContextBytes
}

func (m *Manager) partMaxBytes() int64 {
	if m.PartMaxBytes > 0 {
		return m.PartMaxBytes
	}
	return DefaultPartMaxBytes
}

func (m *Manager) maxPendingPerUser() int {
	if m.MaxPendingPerUser > 0 {
		return m.MaxPendingPerUser
	}
	return defaultMaxPendingPerUser
}

func (m *Manager) maxStoredBytesPerUser() int64 {
	if m.MaxStoredBytesPerUser > 0 {
		return m.MaxStoredBytesPerUser
	}
	return defaultMaxStoredBytesPerUser
}

func (m *Manager) maxStoredBytesTotal() int64 {
	if m.MaxStoredBytesTotal > 0 {
		return m.MaxStoredBytesTotal
	}
	return defaultMaxStoredBytesTotal
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) newID() string {
	if m.IDGen != nil {
		return m.IDGen()
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sub-%d", time.Now().UnixNano())
	}
	return "sub-" + hex.EncodeToString(b[:])
}

func (m *Manager) deriveImageRef(id string) string {
	return fmt.Sprintf("%s/user-uploads/%s:latest", strings.TrimRight(m.Registry, "/"), id)
}

func (m *Manager) deriveContextRef(id string) string {
	if m.ContextBaseURL != "" {
		return fmt.Sprintf("%s/api/v1/internal/submissions/%s/context", strings.TrimRight(m.ContextBaseURL, "/"), id)
	}
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(m.ContextStore, "/"), id, contextBlobName)
}

// OpenContext reads the row before the blob; callers verify the returned digest to detect racing re-uploads.
func (m *Manager) OpenContext(ctx context.Context, id string) (io.ReadCloser, string, error) {
	if m.Blobs == nil {
		return nil, "", ErrUploadsUnavailable
	}
	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, "", err
	}
	rc, err := m.Blobs.Open(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return rc, sub.ContextSHA256, nil
}

// auditDockerfile records provenance only; Kaniko executes the Dockerfile inside the uploaded context.
func auditDockerfile(id, contextRef string) string {
	return fmt.Sprintf(
		"# felis user-modpack submission %s\n"+
			"# The executed Dockerfile is provided by the uploaded build context:\n"+
			"#   %s\n"+
			"# Built in the isolated felis-build sandbox and Trivy-gated (spec §16).\n",
		id, contextRef)
}

type CreateRequest struct {
	DisplayName string
	SubmittedBy string
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Submission, error) {
	name := strings.TrimSpace(req.DisplayName)
	switch {
	case name == "":
		return nil, invalidf("display name is required")
	case len(name) > maxDisplayName:
		return nil, invalidf("display name exceeds %d characters", maxDisplayName)
	case !displayNameRE.MatchString(name):
		return nil, invalidf("display name contains unsupported characters")
	}
	if strings.TrimSpace(req.SubmittedBy) == "" {
		return nil, invalidf("submitter identity is required")
	}

	id := m.newID()
	s := &Submission{
		ID:          id,
		SubmittedBy: req.SubmittedBy,
		DisplayName: name,
		ContextRef:  m.deriveContextRef(id),
		Status:      StatusPendingReview,
		CreatedAt:   m.now(),
	}
	if pending, err := m.Store.CreateSubmission(ctx, s, m.maxPendingPerUser()); errors.Is(err, ErrQuotaExceeded) {
		return nil, fmt.Errorf("%w: %d submissions are already awaiting review (limit %d)",
			ErrQuotaExceeded, pending, m.maxPendingPerUser())
	} else if err != nil {
		return nil, err
	}
	return s, nil
}

// UploadContext permits only owned pending submissions and atomically stores a capped gzip context plus its approval digest.
func (m *Manager) UploadContext(ctx context.Context, id, submittedBy string, r io.Reader) (*Submission, error) {
	if strings.TrimSpace(submittedBy) == "" {
		return nil, invalidf("submitter identity is required")
	}
	if m.Blobs == nil {
		return nil, ErrUploadsUnavailable
	}
	sub, err := m.ownPending(ctx, id, submittedBy)
	if err != nil {
		return nil, err
	}

	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		return nil, invalidf("build context must be a gzip-compressed tarball (.tar.gz)")
	}

	limit, over, err := m.uploadLimit(ctx, submittedBy, id, true)
	if err != nil {
		return nil, err
	}
	if rc, ok := m.Blobs.(RoomChecker); ok {
		if err := rc.CheckRoom(limit); err != nil {
			return nil, err
		}
	}
	h := sha256.New()
	n, err := m.Blobs.Put(ctx, id, io.TeeReader(&cappedReader{r: br, left: limit, over: over}, h))
	if err != nil {
		m.forgetSize(id)
		return nil, err
	}
	m.noteSize(id, n)
	digest := hex.EncodeToString(h.Sum(nil))
	won, err := m.Store.SetContextDigest(ctx, id, digest)
	if err != nil {
		return nil, err
	}
	if !won {
		if _, err := m.Store.GetSubmission(ctx, id); errors.Is(err, ErrNotFound) {
			// A concurrent delete may finish cleanup before this upload lands; remove the orphaned blob here.
			if err := m.deleteBlob(ctx, id); err != nil {
				return nil, err
			}
			return nil, ErrNotFound
		}
		return nil, ErrAlreadyReviewed
	}
	sub.ContextSHA256 = digest
	return sub, nil
}

// ownPending hides other users' IDs as ErrNotFound and rejects reviewed submissions.
func (m *Manager) ownPending(ctx context.Context, id, submittedBy string) (*Submission, error) {
	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.SubmittedBy != submittedBy {
		return nil, ErrNotFound
	}
	if sub.Status != StatusPendingReview {
		return nil, ErrAlreadyReviewed
	}
	return sub, nil
}

// uploadLimit is read-then-write and can overshoot across replicas; each blob stays capped and final completion checks fresh sizes.
func (m *Manager) uploadLimit(ctx context.Context, submittedBy, id string, fresh bool) (int64, error, error) {
	used, total, err := m.storedBytes(ctx, submittedBy, id, fresh)
	if err != nil {
		return 0, nil, err
	}
	remaining, budgetErr := m.maxStoredBytesPerUser()-used, errStorageQuota
	if left := m.maxStoredBytesTotal() - total; left < remaining {
		remaining, budgetErr = left, fmt.Errorf("%w: every user's uploads together reached the %d-byte limit", ErrUploadsFull, m.maxStoredBytesTotal())
	}
	if remaining <= 0 {
		return 0, nil, budgetErr
	}
	limit, over := m.maxContextBytes(), errContextTooLarge
	if remaining < limit {
		limit, over = remaining, budgetErr
	}
	return limit, over, nil
}

type UploadProgress struct {
	Received        int64 `json:"received"`
	PartMaxBytes    int64 `json:"part_max_bytes"`
	MaxContextBytes int64 `json:"max_context_bytes"`
}

func (m *Manager) chunked(ctx context.Context, id, submittedBy string) error {
	if strings.TrimSpace(submittedBy) == "" {
		return invalidf("submitter identity is required")
	}
	if m.Blobs == nil || m.Parts == nil {
		return ErrUploadsUnavailable
	}
	_, err := m.ownPending(ctx, id, submittedBy)
	return err
}

func (m *Manager) UploadStatus(ctx context.Context, id, submittedBy string) (UploadProgress, error) {
	if err := m.chunked(ctx, id, submittedBy); err != nil {
		return UploadProgress{}, err
	}
	n, _, err := m.Parts.Size(id)
	if err != nil {
		return UploadProgress{}, err
	}
	return UploadProgress{Received: n, PartMaxBytes: m.partMaxBytes(), MaxContextBytes: m.maxContextBytes()}, nil
}

// UploadPart resets at offset 0 or requires the current length; parts stay staged until CompleteUpload and share whole-context budgets.
func (m *Manager) UploadPart(ctx context.Context, id, submittedBy string, offset int64, r io.Reader) (UploadProgress, error) {
	if err := m.chunked(ctx, id, submittedBy); err != nil {
		return UploadProgress{}, err
	}
	if offset < 0 {
		return UploadProgress{}, invalidf("offset must not be negative")
	}
	br := bufio.NewReader(r)
	if offset == 0 {
		if magic, err := br.Peek(2); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
			return UploadProgress{}, invalidf("build context must be a gzip-compressed tarball (.tar.gz)")
		}
	}
	limit, over, err := m.uploadLimit(ctx, submittedBy, id, false)
	if err != nil {
		return UploadProgress{}, err
	}
	left, partOver := m.partMaxBytes(), error(ErrPartTooLarge)
	if room := limit - offset; room <= left {
		left, partOver = max(room, 0), over
	}
	if err := m.Parts.CheckRoom(left); err != nil {
		return UploadProgress{}, err
	}
	n, err := m.Parts.Append(id, offset, &cappedReader{r: br, left: left, over: partOver})
	if err != nil {
		return UploadProgress{}, err
	}
	return UploadProgress{Received: n, PartMaxBytes: m.partMaxBytes(), MaxContextBytes: m.maxContextBytes()}, nil
}

// CompleteUpload uses all UploadContext checks and retains staged bytes after failure for retry or stale-part cleanup.
func (m *Manager) CompleteUpload(ctx context.Context, id, submittedBy string) (*Submission, error) {
	if err := m.chunked(ctx, id, submittedBy); err != nil {
		return nil, err
	}
	release, err := m.Parts.hold(id)
	if err != nil {
		return nil, err
	}
	defer release()
	f, err := m.Parts.open(id)
	if err != nil {
		return nil, err
	}
	sub, err := m.UploadContext(ctx, id, submittedBy, f)
	f.Close()
	if err != nil {
		return nil, err
	}
	if err := m.Parts.Delete(id); err != nil {
		log.Printf("submit: %v", err)
	}
	return sub, nil
}

func (m *Manager) ReapStaleParts(olderThan time.Duration) (int, error) {
	if m.Parts == nil {
		return 0, nil
	}
	return m.Parts.Reap(m.now().Add(-olderThan))
}

// storedBytes excludes the replaced ID, counts staged parts, and charges approved contexts only to total storage.
func (m *Manager) storedBytes(ctx context.Context, submittedBy, excludeID string, fresh bool) (user, total int64, err error) {
	subs, err := m.Store.ListSubmissions(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, s := range subs {
		if s.ID == excludeID {
			continue
		}
		n, err := m.blobSize(ctx, s.ID, fresh)
		if err != nil {
			return 0, 0, err
		}
		if m.Parts != nil {
			staged, _, err := m.Parts.Size(s.ID)
			if err != nil {
				return 0, 0, fmt.Errorf("%w: size of the staged upload of %s: %v", ErrStoreUnavailable, s.ID, err)
			}
			n += staged
		}
		total += n
		if s.SubmittedBy == submittedBy && s.Status != StatusApproved {
			user += n
		}
	}
	return user, total, nil
}

func (m *Manager) blobSize(ctx context.Context, id string, fresh bool) (int64, error) {
	now := m.now()
	if !fresh {
		m.sizesMu.Lock()
		k, ok := m.sizes[id]
		m.sizesMu.Unlock()
		if ok && now.Sub(k.at) < blobSizeTTL {
			return k.n, nil
		}
	}
	n, _, err := m.Blobs.Size(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("%w: size of the context of %s: %v", ErrStoreUnavailable, id, err)
	}
	m.noteSizeAt(id, n, now)
	return n, nil
}

func (m *Manager) noteSize(id string, n int64) { m.noteSizeAt(id, n, m.now()) }

func (m *Manager) noteSizeAt(id string, n int64, at time.Time) {
	m.sizesMu.Lock()
	defer m.sizesMu.Unlock()
	if m.sizes == nil {
		m.sizes = map[string]knownSize{}
	}
	m.sizes[id] = knownSize{n: n, at: at}
}

func (m *Manager) forgetSize(id string) {
	m.sizesMu.Lock()
	defer m.sizesMu.Unlock()
	delete(m.sizes, id)
}

// ReapRejected keeps rows and continues after deletion failures, reporting the first error.
func (m *Manager) ReapRejected(ctx context.Context, olderThan time.Duration) (int, error) {
	if m.Blobs == nil {
		return 0, nil
	}
	subs, err := m.Store.ListSubmissions(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := m.now().Add(-olderThan)
	var reaped int
	var firstErr error
	for _, s := range subs {
		if s.Status != StatusRejected || s.ReviewedAt == nil || s.ReviewedAt.After(cutoff) {
			continue
		}
		_, ok, err := m.Blobs.Size(ctx, s.ID)
		if err == nil && ok {
			if err = m.Blobs.Delete(ctx, s.ID); err == nil {
				m.noteSize(s.ID, 0)
				reaped++
			}
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("submit: reap the context of rejected submission %s: %w", s.ID, err)
		}
	}
	return reaped, firstErr
}

// cappedReader probes one byte beyond the limit to distinguish exact-size acceptance from overflow.
type cappedReader struct {
	r    io.Reader
	left int64
	over error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			return 0, c.over
		}
		if err == nil {
			return 0, io.EOF
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// Approve validates before CAS and starts a build only for the winner; approval never bypasses the scan gate.
func (m *Manager) Approve(ctx context.Context, id, reviewedBy, expectedDigest string) (*Submission, error) {
	if strings.TrimSpace(reviewedBy) == "" {
		return nil, invalidf("reviewer identity is required")
	}
	expectedDigest = strings.ToLower(strings.TrimSpace(expectedDigest))
	if m.Blobs != nil && !build.IsSHA256Hex(expectedDigest) {
		return nil, invalidf("expected_digest must be the sha256 of the context you reviewed (64 hex characters)")
	}

	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusPendingReview {
		return nil, ErrAlreadyReviewed
	}

	// Check blob presence before committing approval so missing contexts remain pending.
	if m.Blobs != nil {
		ok, err := m.Blobs.Exists(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, invalidf("no build context has been uploaded for this submission")
		}
		if sub.ContextSHA256 == "" {
			return nil, invalidf("this context was uploaded before digests were recorded; the submitter must upload it again")
		}
		if sub.ContextSHA256 != expectedDigest {
			return nil, ErrContextChanged
		}
	}

	imageRef := m.deriveImageRef(id)
	req := build.Request{
		ImageRef:    imageRef,
		Dockerfile:  auditDockerfile(id, sub.ContextRef),
		ContextRef:  sub.ContextRef,
		BaseImage:   "",
		RequestedBy: reviewedBy,
	}
	// Pin HTTP context fetches to the reviewed digest so an already-streaming re-upload cannot substitute unseen bytes.
	if build.IsHTTPContextRef(sub.ContextRef) {
		req.ContextDigest = sub.ContextSHA256
	}
	// Prevalidate with the Builder's registry before CAS to avoid stranding approved rows on deterministic config errors.
	if err := build.Validate(req, build.Config{RegistryURL: m.Registry, ContextOrigin: m.ContextBaseURL}); err != nil {
		return nil, err
	}

	now := m.now()
	won, err := m.Store.ApproveSubmission(ctx, id, reviewedBy, imageRef, sub.ContextSHA256, now)
	if err != nil {
		return nil, err
	}
	if !won {
		if cur, err := m.Store.GetSubmission(ctx, id); err == nil && cur.Status == StatusPendingReview {
			return nil, ErrContextChanged
		}
		return nil, ErrAlreadyReviewed
	}

	bld, err := m.Builds.Submit(ctx, req)
	if err != nil {
		// Submit failed before starting any build; direct-build remediation is safe.
		return nil, fmt.Errorf("submit: approved but build hand-off failed: %w", err)
	}
	if err := m.Store.LinkBuild(ctx, id, bld.ID); err != nil {
		// LinkBuild failed after startup; return the running build ID so remediation cannot accidentally double-build.
		return nil, fmt.Errorf("submit: approved and build %s started but linking it to the submission failed (build is running — do NOT re-submit, reconcile build_id by hand): %w", bld.ID, err)
	}

	reviewedAt := now
	sub.Status = StatusApproved
	sub.ImageRef = imageRef
	sub.BuildID = bld.ID
	sub.ReviewedBy = reviewedBy
	sub.ReviewedAt = &reviewedAt
	return sub, nil
}

func (m *Manager) Reject(ctx context.Context, id, reviewedBy, reason string) (*Submission, error) {
	if strings.TrimSpace(reviewedBy) == "" {
		return nil, invalidf("reviewer identity is required")
	}
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return nil, invalidf("a reject reason is required")
	case len(reason) > maxRejectReason:
		return nil, invalidf("reject reason exceeds %d characters", maxRejectReason)
	}

	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusPendingReview {
		return nil, ErrAlreadyReviewed
	}

	now := m.now()
	won, err := m.Store.RejectSubmission(ctx, id, reviewedBy, reason, now)
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, ErrAlreadyReviewed
	}

	reviewedAt := now
	sub.Status = StatusRejected
	sub.ReviewedBy = reviewedBy
	sub.RejectReason = reason
	sub.ReviewedAt = &reviewedAt
	return sub, nil
}

// Withdraw atomically checks ownership and pending status before deleting the row and context.
func (m *Manager) Withdraw(ctx context.Context, id, submittedBy string) (*Submission, error) {
	if strings.TrimSpace(submittedBy) == "" {
		return nil, invalidf("submitter identity is required")
	}
	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.SubmittedBy != submittedBy {
		return nil, ErrNotFound
	}
	if sub.Status != StatusPendingReview {
		return nil, ErrAlreadyReviewed
	}
	won, err := m.Store.DeletePendingSubmission(ctx, id, submittedBy)
	if err != nil {
		return nil, err
	}
	if !won {
		// A lost review or delete race must not reap a context now owned by the winning outcome.
		return nil, ErrAlreadyReviewed
	}
	if err := m.deleteBlob(ctx, id); err != nil {
		return nil, err
	}
	return sub, nil
}

func (m *Manager) Delete(ctx context.Context, id string) (*Submission, error) {
	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	deleted, err := m.Store.DeleteSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if !deleted {
		return nil, ErrNotFound
	}
	if err := m.deleteBlob(ctx, id); err != nil {
		return nil, err
	}
	return sub, nil
}

// deleteBlob runs after row removal; cleanup errors may leave an orphaned blob but cannot leave a live row without context.
func (m *Manager) deleteBlob(ctx context.Context, id string) error {
	if m.Parts != nil {
		if err := m.Parts.Delete(id); err != nil {
			return fmt.Errorf("submit: submission removed, but its unfinished upload could not be deleted: %w", err)
		}
	}
	if m.Blobs == nil {
		return nil
	}
	err := m.Blobs.Delete(ctx, id)
	m.forgetSize(id)
	if err != nil {
		return fmt.Errorf("submit: submission removed, but its uploaded context could not be deleted (it may remain on the uploads store): %w", err)
	}
	return nil
}

func (m *Manager) List(ctx context.Context, opts ListOpts) (Page, error) {
	opts.SubmittedBy = ""
	return m.page(ctx, opts)
}

// ListBy scopes by submittedBy regardless of opts.
func (m *Manager) ListBy(ctx context.Context, submittedBy string, opts ListOpts) (Page, error) {
	if strings.TrimSpace(submittedBy) == "" {
		return Page{}, invalidf("submitter identity is required")
	}
	opts.SubmittedBy = submittedBy
	return m.page(ctx, opts)
}

func (m *Manager) page(ctx context.Context, opts ListOpts) (Page, error) {
	switch opts.Status {
	case "", StatusPendingReview, StatusApproved, StatusRejected:
	default:
		return Page{}, invalidf("unknown status %q", opts.Status)
	}
	opts.Query = strings.TrimSpace(opts.Query)
	if opts.Limit <= 0 {
		opts.Limit = DefaultListLimit
	}
	opts.Limit = min(opts.Limit, MaxListLimit)
	opts.Offset = max(opts.Offset, 0)
	return m.Store.PageSubmissions(ctx, opts)
}

var _ Builds = (*build.Builder)(nil)
