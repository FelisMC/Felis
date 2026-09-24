// Package submit implements the user-uploaded modpack approval lane.
//
// PROVENANCE — read this before trusting the spec citations elsewhere. This lane
// is a USER-DIRECTED extension, not a feature of Felis-Spec-V4.1 as written. The
// spec's §16 build subsystem deliberately DELETED a manual-approve flow ("简化掉
// (admin 信任,删):人工 approve 流程 → 删 ... 无人工 approve 端点") under a trust
// model where "上传仅 SysAdmin" (§1 invariant 9: "build 鉴权简化(admin-only)").
// The product owner subsequently required the opposite: ordinary, untrusted
// users may upload their own modpacks, and an admin must approve each one before
// it is usable. Re-introducing untrusted-origin uploads is exactly the condition
// under which §16's deleted gate must come BACK — the spec removed the approve
// step only because it had assumed every uploader was already a trusted SysAdmin.
// So this lane departs from §16's *ingress* simplification on purpose, with the
// owner's instruction as the authority; it does NOT relax anything §16 calls
// non-negotiable. (Do not cite "spec §8" for this lane — §8 is Readiness/优雅关闭
// /Console. Earlier drafts mis-cited it; those citations have been corrected.)
//
// What is preserved (§16, non-negotiable). The build subsystem treats the
// Dockerfile as hostile at runtime (build/build.go): Kaniko reads the Dockerfile
// from inside the uploaded context, so every build — admin or user — runs in the
// isolated felis-build namespace under a weak service account behind a default-
// deny egress policy, and a CRITICAL CVE fails the Trivy gate ("运行时隔离不能
// 省"). This lane changes none of that: an approved submission is built through
// the SAME gated Builder.Submit. The approval gate is layered in FRONT of the
// still-mandatory Trivy scan, never instead of it — a human "yes" does not skip
// the scan, so a CRITICAL CVE still blocks admission after approval.
//
// Two further guards close the gap an untrusted origin opens:
//
//   - The push target (image_ref) is DERIVED by the Manager as
//     {registry}/user-uploads/{submissionID}:latest — a namespace platform refs
//     never use, so a submitter can neither collide with nor target the platform
//     image namespace.
//   - The build context (context_ref) is DERIVED as {contextStore}/{id}/...
//     too, NOT taken as free-form user input. Kaniko interpolates the context
//     ref straight into its `--context=` argument (build/jobspec.go); a user-
//     chosen ref would let an untrusted origin point the build at an arbitrary
//     source. By deriving it from the submission id the user selects nothing
//     that reaches the executor — only the modpack blob behind the pinned,
//     id-namespaced location (uploaded through the Blobs transport, and served
//     back to the build Pod over the service-token-gated internal face).
//
// Source of truth. submissions is a NEW Postgres business-truth domain, added to
// §1 invariant 2's enumeration (owner/claim/accounts/audit/quota/images/builds/
// backups) by the same owner instruction that introduced this lane. The
// image_submissions row is the business authority — the approval record — while
// the build is the execution record in image_builds. Approve never copies the
// build's authoritative fields; it records the derived push target it asked the
// Builder to build plus the reviewer identity, and links the build id once
// Submit succeeds.
//
// The Manager depends on the Store and Builds interfaces, so creation, the
// approve CAS, rejection, and build hand-off are all unit-tested against
// in-memory fakes. The Postgres implementation (PGStore) compiles here but is
// exercised only by integration tests against a live database.
package submit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"felis.lolicon.best/internal/build"
)

// Status mirrors the submission_status enum (spec §6, migration 0002).
type Status string

const (
	StatusPendingReview Status = "pending_review"
	StatusApproved      Status = "approved"
	StatusRejected      Status = "rejected"
)

// Sentinels. The API layer maps ErrInvalid→400, ErrNotFound→404,
// ErrAlreadyReviewed→409 and ErrUploadsUnavailable→503; they are kept distinct
// from store/cluster failures so those surface as 500.
var (
	ErrInvalid         = errors.New("submit: invalid request")
	ErrNotFound        = errors.New("submit: submission not found")
	ErrAlreadyReviewed = errors.New("submit: submission already reviewed")
	// ErrQuotaExceeded reports that the caller's upload allowance is spent —
	// either too many of their submissions are already awaiting review, or their
	// stored contexts already fill the per-user byte budget. The request is not
	// malformed; the allowance is exhausted. The API maps it to 403, matching the
	// server-resource quota's status (spec §7).
	ErrQuotaExceeded = errors.New("submit: quota exceeded")
	// ErrUploadsUnavailable means this deployment configured a context store with
	// no implemented upload transport (a nil Manager.Blobs — e.g. an object-store
	// base with no client wired). UploadContext returns it so the endpoint reports
	// an honest 503, never a 500, exactly as the restore executor does when its
	// integration is not wired.
	ErrUploadsUnavailable = errors.New("submit: context upload transport not configured")
	// ErrBlobNotFound reports that a submission has no stored context blob (or it
	// was never uploaded). The internal context-fetch route maps it to 404, the
	// same distinction Exists draws for Approve.
	ErrBlobNotFound = errors.New("submit: context blob not found")
)

// invalidf wraps ErrInvalid so every malformed-request case maps to one 400.
func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// errContextTooLarge trips when an upload exceeds the size cap. It wraps
// ErrInvalid so an oversize upload maps to a 400; the storage layer's %w wrapping
// preserves that chain through to the API error mapper.
var errContextTooLarge = fmt.Errorf("%w: build context exceeds the maximum allowed size", ErrInvalid)

// errStorageQuota trips when an upload would push the user past their per-user
// storage budget. It wraps ErrQuotaExceeded so the API answers 403, distinctly
// from errContextTooLarge's 400: the blob is fine, the allowance is spent.
var errStorageQuota = fmt.Errorf("%w: the upload exceeds your remaining storage allowance", ErrQuotaExceeded)

const (
	maxDisplayName  = 200
	maxRejectReason = 1000
	// defaultMaxContextBytes caps an uploaded build-context blob. Modpack contexts
	// (mods, configs, an occasional bundled world) are large, so the cap is
	// generous; it bounds what one untrusted upload can write to the uploads PVC,
	// one blob at a time. The per-user budget below bounds the SUM across a user's
	// uploads; this cap is what keeps any single write bounded. Override per-Manager
	// via MaxContextBytes.
	defaultMaxContextBytes = 1 << 30 // 1 GiB
	// defaultMaxPendingPerUser caps how many pending_review submissions one user
	// may hold at once. Untrusted ingress has no natural bound — a logged-in
	// player could otherwise file rows all day — and every pending row is a
	// review-queue item an admin has to read, so the cap is small on purpose:
	// enough to stage a couple of packs, far short of a flood. Override per
	// Manager via MaxPendingPerUser.
	defaultMaxPendingPerUser = 5
	// defaultMaxStoredBytesPerUser caps the total bytes one user's stored
	// contexts may occupy on the uploads store. The uploads PVC renders at a
	// fixed 5Gi (platform/workloads.go); without a per-user budget one account
	// could fill it and every other user's upload would start failing. Two GiB
	// leaves room for a couple of full-size modpacks (a single blob may be 1 GiB)
	// while keeping a small user base from exhausting the volume; size the PVC
	// above users × this budget before raising it. Override per Manager via
	// MaxStoredBytesPerUser.
	defaultMaxStoredBytesPerUser = 2 << 30 // 2 GiB
)

// displayNameRE constrains the user-supplied label to a calm, single-line set:
// it is the only free-form string a submission carries, and although JSON
// encoding already neutralises it in responses, rejecting control characters
// and odd whitespace here keeps it clean in the admin review queue and any log
// line. Length is bounded separately so this stays a charset check.
var displayNameRE = regexp.MustCompile(`^[\p{L}\p{N} ._:()\[\]/+-]+$`)

// Submission mirrors an image_submissions row (spec §6, migration 0002): a
// user's request to have a modpack built into a runnable image, plus the admin
// review verdict. The user supplies only DisplayName; ContextRef/ImageRef/
// BuildID/ReviewedBy are filled by the platform, never by the submitter.
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
}

// Store is the business-layer persistence the Manager depends on. It is an
// interface so the Manager is tested against an in-memory fake; the Postgres
// implementation (PGStore) is integration-tested only.
type Store interface {
	// CreateSubmission inserts a pending_review row.
	CreateSubmission(ctx context.Context, s *Submission) error
	// CountPendingSubmissionsBy reports how many of one user's submissions are
	// still pending_review — the queue-length read behind the per-user pending
	// cap in Create.
	CountPendingSubmissionsBy(ctx context.Context, submittedBy string) (int, error)
	// GetSubmission loads one submission, or ErrNotFound.
	GetSubmission(ctx context.Context, id string) (*Submission, error)
	// ListSubmissions returns every submission, newest first (admin queue).
	ListSubmissions(ctx context.Context) ([]Submission, error)
	// ListSubmissionsBy returns one user's submissions, newest first.
	ListSubmissionsBy(ctx context.Context, submittedBy string) ([]Submission, error)
	// ApproveSubmission atomically flips pending_review -> approved, recording the
	// derived image_ref, the reviewer and reviewed_at. It reports whether THIS
	// call won the transition: false means a concurrent review already moved the
	// row, so the caller must NOT start a build.
	ApproveSubmission(ctx context.Context, id, reviewedBy, imageRef string, at time.Time) (won bool, err error)
	// RejectSubmission atomically flips pending_review -> rejected, recording the
	// reviewer, the reason and reviewed_at. Reports whether THIS call won.
	RejectSubmission(ctx context.Context, id, reviewedBy, reason string, at time.Time) (won bool, err error)
	// LinkBuild records the build id on an approved submission. It runs only after
	// Builder.Submit succeeds, so the implication is ONE-WAY: build_id non-null ⇒
	// a build started. The converse does NOT hold — if Submit succeeds but this
	// link write then fails, build_id stays NULL while a build is genuinely running
	// (Approve surfaces that distinctly so remediation does not double-build; see
	// the Approve ordering note and the package KNOWN-LIMITATION).
	LinkBuild(ctx context.Context, id, buildID string) error
}

// Builds is the slice of the build subsystem the approval lane drives. An
// approved submission is built through the SAME gated Builder.Submit as an
// admin's direct build, so the Trivy scan-gate applies identically (spec §16).
type Builds interface {
	Submit(ctx context.Context, req build.Request) (*build.Build, error)
}

// Blobs is the build-context blob transport the lane depends on to place a
// submitter's uploaded modpack at the platform-derived, id-namespaced location
// deriveContextRef points Kaniko at. Creation only derives and records the ref;
// the bytes behind it arrive through Put here, and the build Pod reads them back
// through Open (the manager exposes it as OpenContext, which the API's internal
// context route serves). It is an interface so the Manager is unit-tested against
// an in-memory fake; the production implementations are LocalContextStore
// (filesystem) and S3ContextStore (object store).
//
// Both methods key off the submission id, never a caller-supplied path, so the
// write target is as platform-pinned as the derived ref itself. Put stores (and
// atomically overwrites, while the submission is still pending) the blob; Exists
// reports whether one has been stored, so Approve can refuse to build a
// submission whose context was never uploaded.
type Blobs interface {
	Put(ctx context.Context, id string, r io.Reader) (int64, error)
	Exists(ctx context.Context, id string) (bool, error)
	// Size returns the stored blob's size in bytes; ok=false means no blob is
	// stored for id. UploadContext sums this over a user's submissions to enforce
	// the per-user storage budget, so it must report what is actually on the
	// store — never a recorded number that could drift from it (a re-upload
	// supersedes the previous blob in place).
	Size(ctx context.Context, id string) (int64, bool, error)
	// Open returns the stored blob's bytes for the internal context-fetch route
	// the build Pod's initContainer dials (cmd/felis fetch-context). It returns an
	// error wrapping ErrBlobNotFound when no blob exists, so the route can answer
	// 404 without leaking which ids do exist.
	Open(ctx context.Context, id string) (io.ReadCloser, error)
}

// Manager orchestrates the approval lane. It holds no mutable state; the clock
// and id generator are injectable for hermetic tests.
type Manager struct {
	Store  Store
	Builds Builds

	// Registry is the internal registry host[:port] the derived push target
	// addresses. It MUST be the SAME value the Builder is configured with
	// (build.Config.RegistryURL): the Manager pre-validates the derived ref
	// against this host before the approve CAS, and the Builder re-validates
	// against its own config at Submit — if the two disagree the pre-check passes
	// but Submit rejects, stranding an approved row. cmd/felis wires both from one
	// field. The derived ref is {Registry}/user-uploads/{id}:latest.
	Registry string
	// ContextStore is the pinned Kaniko build-context base for user uploads, e.g.
	// "s3://felis-user-uploads" (an object store) or a local uploads PVC path. The
	// derived context ref is {ContextStore}/{id}/context.tar.gz.
	ContextStore string
	// ContextBaseURL, when set, is the platform's internal-face base URL
	// (platform.InternalAPIBaseURL). It makes the derived context ref an HTTP URL
	// on that face — {ContextBaseURL}/api/v1/internal/submissions/{id}/context —
	// instead of a filesystem/object-store location: the build Pod cannot mount
	// the uploads PVC (builds run in another namespace) and carries no object-store
	// credentials, so the API streams the blob it stored at ContextStore over the
	// service-token-gated internal face. Empty keeps the legacy ref shape for a
	// deployment that predates the transport.
	ContextBaseURL string
	// Blobs is the upload transport that persists the modpack behind the derived
	// context ref. When nil (a store with no implemented transport, e.g. an
	// object-store base with no client), UploadContext returns ErrUploadsUnavailable
	// so the endpoint reports 503. Its backing MUST match ContextStore so the blob
	// lands exactly where the derived ref points.
	Blobs Blobs
	// MaxContextBytes overrides the uploaded-context size cap; 0 uses
	// defaultMaxContextBytes.
	MaxContextBytes int64
	// MaxPendingPerUser overrides how many of one user's submissions may await
	// review at once; 0 uses defaultMaxPendingPerUser.
	MaxPendingPerUser int
	// MaxStoredBytesPerUser overrides the per-user stored-context budget; 0 uses
	// defaultMaxStoredBytesPerUser.
	MaxStoredBytesPerUser int64

	Now   func() time.Time
	IDGen func() string
}

func (m *Manager) maxContextBytes() int64 {
	if m.MaxContextBytes > 0 {
		return m.MaxContextBytes
	}
	return defaultMaxContextBytes
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
	// The id namespaces the derived image ref (deriveImageRef) and is the PK of
	// image_submissions, so it must be lowercase (imageNameRE in build/validate.go),
	// path/argv-safe, and collision-resistant. 8 bytes of crypto/rand hex give all
	// three: lowercase hex never contains a slash or shell metachar, and the id is
	// unguessable rather than a sequential timestamp. crypto/rand.Read only fails on
	// a broken entropy source; fall back to a nanosecond stamp so creation degrades
	// instead of panicking (a PK clash would surface as a normal insert error).
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sub-%d", time.Now().UnixNano())
	}
	return "sub-" + hex.EncodeToString(b[:])
}

// deriveImageRef is the platform-controlled push target for a submission: a pure
// function of the registry and the submission id, in a user-uploads/ path no
// platform ref uses, so an untrusted submitter can never collide with or target
// the platform image namespace.
func (m *Manager) deriveImageRef(id string) string {
	return fmt.Sprintf("%s/user-uploads/%s:latest", strings.TrimRight(m.Registry, "/"), id)
}

// deriveContextRef is the platform-controlled build context for a submission.
// Like the image ref it is a pure function of the submission id, so the user
// selects nothing that reaches Kaniko's --context argument; only the blob behind
// this pinned, id-namespaced location (placed by the upload transport) varies.
func (m *Manager) deriveContextRef(id string) string {
	if m.ContextBaseURL != "" {
		return fmt.Sprintf("%s/api/v1/internal/submissions/%s/context", strings.TrimRight(m.ContextBaseURL, "/"), id)
	}
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(m.ContextStore, "/"), id, contextBlobName)
}

// OpenContext returns the stored build context for id — the read path behind the
// internal context-fetch route. It requires the upload transport (Blobs): with no
// transport there is no blob to read, so it reports ErrUploadsUnavailable, the
// same honest 503 the upload endpoint gives.
func (m *Manager) OpenContext(ctx context.Context, id string) (io.ReadCloser, error) {
	if m.Blobs == nil {
		return nil, ErrUploadsUnavailable
	}
	return m.Blobs.Open(ctx, id)
}

// auditDockerfile is the audit-archive Dockerfile recorded on the build row. It
// is NOT what Kaniko executes — Kaniko reads the real Dockerfile from inside the
// uploaded context (build/jobspec.go) — so this honestly documents the
// provenance instead of implying a recipe the platform controls. It satisfies
// build.Validate's non-empty requirement.
func auditDockerfile(id, contextRef string) string {
	return fmt.Sprintf(
		"# felis user-modpack submission %s\n"+
			"# The executed Dockerfile is provided by the uploaded build context:\n"+
			"#   %s\n"+
			"# Built in the isolated felis-build sandbox and Trivy-gated (spec §16).\n",
		id, contextRef)
}

// CreateRequest is the validated user upload. The user supplies ONLY a
// human-friendly name; identity comes from the authenticated principal and the
// build inputs (image/context refs) are platform-derived, never from the body.
type CreateRequest struct {
	DisplayName string
	SubmittedBy string
}

// Create records a new pending_review submission. It does NOT start a build:
// nothing is built until an admin approves (the whole point of the lane).
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

	// Per-user pending cap: every pending row is a review-queue item an admin
	// must read, so one account may not park an unbounded number of them. The
	// check and the insert are not atomic (two concurrent creates may jointly
	// land one row over the cap); that is a soft overshoot of a queue-length
	// lever, not a resource bound, so it is deliberately not worth a lock.
	if pending, err := m.Store.CountPendingSubmissionsBy(ctx, req.SubmittedBy); err != nil {
		return nil, err
	} else if pending >= m.maxPendingPerUser() {
		return nil, fmt.Errorf("%w: %d submissions are already awaiting review (limit %d)",
			ErrQuotaExceeded, pending, m.maxPendingPerUser())
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
	if err := m.Store.CreateSubmission(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// UploadContext stores the caller's uploaded modpack as the build context for
// their OWN pending submission — the blob transport the package doc calls
// deferred. It places the bytes at exactly deriveContextRef(id), the platform-
// pinned, id-namespaced location Kaniko reads via --context, so the submitter
// selects nothing that reaches the executor beyond the modpack itself. The row
// is not mutated (there is no "uploaded" column): the blob store is the source of
// truth for presence, which Approve consults via Blobs.Exists.
//
// The gates mirror the lane's trust model:
//   - only the submitter may upload; another user's id is invisible (404, not
//     403) so this endpoint cannot probe other users' submissions;
//   - the context is mutable ONLY while pending_review — once approved the build
//     has already consumed it, once rejected it is dead;
//   - the body must be a gzip tarball (context.tar.gz) and is size-capped, so a
//     wrong-format or oversize upload is rejected as a 400 without persisting;
//   - a user's stored contexts are budgeted (MaxStoredBytesPerUser): the write
//     is capped at the remaining budget, so an upload that would exceed it is
//     refused as a spent allowance (403) before the excess is persisted.
//
// A re-upload while still pending atomically supersedes the previous blob, so a
// user can fix their pack before an admin reviews it.
func (m *Manager) UploadContext(ctx context.Context, id, submittedBy string, r io.Reader) (*Submission, error) {
	if strings.TrimSpace(submittedBy) == "" {
		return nil, invalidf("submitter identity is required")
	}
	if m.Blobs == nil {
		return nil, ErrUploadsUnavailable
	}

	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.SubmittedBy != submittedBy {
		// Not the owner: invisible, so the endpoint cannot confirm the id exists.
		return nil, ErrNotFound
	}
	if sub.Status != StatusPendingReview {
		return nil, ErrAlreadyReviewed
	}

	// Sniff the gzip magic before touching the store so a wrong-format upload fails
	// fast, without persisting anything or reading the whole body.
	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		return nil, invalidf("build context must be a gzip-compressed tarball (.tar.gz)")
	}

	// Per-user storage budget: sum the bytes this user's OTHER submissions
	// already hold (excluding this id, whose blob a re-upload supersedes) and cap
	// the write at whatever remains. cappedReader trips on the first byte past
	// the limit, so the store never persists a blob that would exceed the budget
	// (it removes its temp file on the copy error) and the failure surfaces as a
	// 403, not a 500. The read-then-write pair is not atomic in this package: a
	// burst that reaches two api replicas (or any direct caller of the Manager)
	// can overshoot by up to one blob per interleaved upload — each write still
	// bounded by the single-blob cap — while the API's per-user upload
	// reservation collapses the single-replica case.
	used, err := m.storedBytes(ctx, submittedBy, id)
	if err != nil {
		return nil, err
	}
	remaining := m.maxStoredBytesPerUser() - used
	if remaining <= 0 {
		return nil, errStorageQuota
	}
	limit, over := m.maxContextBytes(), errContextTooLarge
	if remaining < limit {
		// The budget binds before the single-blob cap: an upload tripping here is
		// refused as a spent allowance, never as a malformed request.
		limit, over = remaining, errStorageQuota
	}
	if _, err := m.Blobs.Put(ctx, id, &cappedReader{r: br, left: limit, over: over}); err != nil {
		return nil, err
	}
	return sub, nil
}

// storedBytes sums the stored-blob sizes of submittedBy's submissions, excluding
// excludeID — the submission a pending re-upload is about to replace, whose
// bytes must not be counted twice. Sizes are read from the blob store itself,
// the same source of truth uploads/approval consult, so the sum cannot drift
// from what is actually occupying the volume (including blobs uploaded before
// any budget existed).
func (m *Manager) storedBytes(ctx context.Context, submittedBy, excludeID string) (int64, error) {
	subs, err := m.Store.ListSubmissionsBy(ctx, submittedBy)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, s := range subs {
		if s.ID == excludeID {
			continue
		}
		n, ok, err := m.Blobs.Size(ctx, s.ID)
		if err != nil {
			return 0, err
		}
		if ok {
			total += n
		}
	}
	return total, nil
}

// cappedReader passes through at most left bytes; the first byte beyond the limit
// trips over — errContextTooLarge for the single-blob cap, errStorageQuota when
// the per-user budget binds first. It reads one probe byte past the limit to tell
// an exactly-at-limit blob (accepted) from a larger one (rejected), so a stream
// of exactly the cap is never falsely rejected.
type cappedReader struct {
	r    io.Reader
	left int64
	over error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// At the limit: peek one more byte. Any further data means too large; EOF
		// means the blob was exactly the cap.
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

// Approve is the admin gate. It atomically claims the pending_review -> approved
// transition (CAS) and ONLY the winner starts the build, so concurrent approvals
// can never double-build. The build runs through the SAME gated Builder.Submit as
// an admin's direct build: approval is layered in FRONT of the Trivy scan, never
// instead of it (spec §16), so a CRITICAL CVE still fails the build and nothing
// is admitted even after a human approved.
//
// Ordering and its two post-CAS partial states. Deterministic validation (a
// misconfigured registry/context, an id that does not form a valid ref) runs
// BEFORE the CAS via build.Validate, so such failures never leave a stuck
// approved row. The CAS is then committed before Submit. Two failures can occur
// after it, and they are NOT equally benign:
//
//   - Submit fails (transient cluster error): the row is approved with build_id
//     NULL and NO build is running. Benign and recoverable — an admin re-drives
//     it via the direct build path.
//   - Submit SUCCEEDS but the follow-up LinkBuild fails: the row is approved with
//     build_id NULL while a build IS running and will push to the derived
//     image_ref. On the row this is indistinguishable from the benign case, so a
//     blind "re-drive" would double-build and double-push to :latest (no unique
//     constraint stops it). Approve therefore returns a DISTINCT error naming the
//     running build id and warning not to re-submit.
//
// Both are labeled KNOWN-LIMITATIONs (see package doc); the second is the worse
// one and the reason the error is differentiated. The alternative ordering
// (Submit-then-CAS) would either double-build under a concurrent approve or
// orphan a build on a lost race, both worse still.
func (m *Manager) Approve(ctx context.Context, id, reviewedBy string) (*Submission, error) {
	if strings.TrimSpace(reviewedBy) == "" {
		return nil, invalidf("reviewer identity is required")
	}

	sub, err := m.Store.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusPendingReview {
		return nil, ErrAlreadyReviewed
	}

	// Refuse to approve a submission whose build context was never uploaded: the
	// derived context ref would point Kaniko at nothing, failing the build after a
	// committed CAS. This deterministic check runs BEFORE the CAS (like the
	// build.Validate below), so a missing blob leaves the row pending, never
	// stranded in approved. Skipped when no transport is wired (Blobs nil): the
	// deferred/object-store case cannot be checked here and must not block approve.
	if m.Blobs != nil {
		ok, err := m.Blobs.Exists(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, invalidf("no build context has been uploaded for this submission")
		}
	}

	imageRef := m.deriveImageRef(id)
	req := build.Request{
		ImageRef:    imageRef,
		Dockerfile:  auditDockerfile(id, sub.ContextRef),
		ContextRef:  sub.ContextRef,
		BaseImage:   "", // declared inside the uploaded context, unknown here
		RequestedBy: reviewedBy,
	}
	// Pre-validate against the SAME registry the Builder enforces, BEFORE the CAS,
	// so a deterministic config error cannot strand the row in approved.
	if err := build.Validate(req, build.Config{RegistryURL: m.Registry}); err != nil {
		return nil, err
	}

	now := m.now()
	won, err := m.Store.ApproveSubmission(ctx, id, reviewedBy, imageRef, now)
	if err != nil {
		return nil, err
	}
	if !won {
		// Lost the race to a concurrent approve/reject.
		return nil, ErrAlreadyReviewed
	}

	// Only the CAS winner starts the build.
	bld, err := m.Builds.Submit(ctx, req)
	if err != nil {
		// Approved but unbuilt and NOTHING is running — admin remediation via the
		// direct build path is safe (the benign partial state).
		return nil, fmt.Errorf("submit: approved but build hand-off failed: %w", err)
	}
	if err := m.Store.LinkBuild(ctx, id, bld.ID); err != nil {
		// The worse partial state: the build IS running and will push to image_ref,
		// but the row still shows build_id NULL. A blind re-drive would double-build.
		// Name the running build id and warn explicitly so an operator reconciles it
		// (link it by hand / let it finish) instead of re-submitting.
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

// Reject is the admin's other verdict: it atomically claims
// pending_review -> rejected with a required reason and starts NO build.
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

// List returns every submission, newest first (the admin review queue).
func (m *Manager) List(ctx context.Context) ([]Submission, error) {
	return m.Store.ListSubmissions(ctx)
}

// ListBy returns one user's submissions, newest first (the "my uploads" view).
func (m *Manager) ListBy(ctx context.Context, submittedBy string) ([]Submission, error) {
	if strings.TrimSpace(submittedBy) == "" {
		return nil, invalidf("submitter identity is required")
	}
	return m.Store.ListSubmissionsBy(ctx, submittedBy)
}

// Compile-time proof that the production build subsystem satisfies Builds.
var _ Builds = (*build.Builder)(nil)
