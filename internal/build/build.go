// Package build implements the image build subsystem (spec §16) — "the
// platform's biggest security surface". A SysAdmin uploads a Dockerfile and a
// context tarball; felis-api starts an in-cluster Kaniko Job that builds and
// pushes to the internal registry, after which a Trivy scan gates admission to
// the image whitelist.
//
// Trust model (spec §16, §22): we trust the SysAdmin at the *ingress* (only an
// admin through Zero Trust may submit a build) but never trust the *Dockerfile
// at runtime* — an arbitrary Dockerfile is build-time RCE whose victim is the
// cluster, not the uploader. So the build Pod runs with a deliberately weak
// service account in an isolated namespace that can only push to the registry
// and cannot touch the minecraft namespace, the felis database, or the K8s API
// (spec §21). Those isolation guarantees live in the Job/NetworkPolicy specs
// (jobspec.go) and are asserted by unit tests, since no cluster runs here.
//
// The Trivy gate is enforced as the build Pod's *exit code*: a kaniko
// initContainer builds and pushes, then a trivy container scans the pushed ref
// with `--exit-code 1 --severity CRITICAL`. Therefore "Job Succeeded" is
// equivalent to "pushed AND no CRITICAL CVE". felis-api observes the Job phase
// and performs the database writes — the build Pod itself never has database
// credentials (the weak-SA red line). On success the image is admitted to
// image_whitelist with enabled=true (recording added_by); on failure the build
// is marked failed and nothing is admitted (spec §16: the only retained
// automatic gate).
//
// The Builder depends on the Store and Jobs interfaces, so submission, the
// scan-gate translation, cancellation, and image admission are all unit-tested
// against in-memory fakes. The Postgres (pgStore) and controller-runtime
// (k8sJobs) implementations compile here but are exercised only by integration
// tests against a live database / cluster.
package build

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/metrics"
)

// Status mirrors the build_status enum (spec §6).
type Status string

const (
	StatusPending   Status = "pending"
	StatusBuilding  Status = "building"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// terminal reports whether a status is final and no longer reconciled.
func (s Status) terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// JobPhase is the build Pod's lifecycle as observed from the K8s Job, decoupled
// from any K8s type so the scan-gate translation stays unit-testable.
type JobPhase int

const (
	// JobUnknown means the Job was not found (e.g. GC'd); treated as failed.
	JobUnknown JobPhase = iota
	JobPending
	JobRunning
	// JobSucceeded means kaniko pushed AND trivy found no CRITICAL CVE — the
	// scan gate passed (spec §16).
	JobSucceeded
	// JobFailed means kaniko failed OR trivy found a CRITICAL CVE — the build
	// is rejected and nothing is admitted.
	JobFailed
)

// image admission sources (spec §6 image_whitelist.source). The column is plain
// text, not an enum, so a curated value costs no schema change.
const (
	SourceBuilt    = "built"
	SourceExternal = "external"
	// SourceRecommended marks a platform-curated whitelist entry: an image Felis
	// itself ships and vouches for, which the create-server form may surface ahead
	// of the rest. It is a PRESENTATION marker only — admission still turns solely
	// on enabled (see ImageAdmitted), so a recommended row is admitted by exactly
	// the same rule as any other and carries no extra privilege.
	//
	// The bar for this marker is joinability, not popularity. Velocity runs
	// proxy-wide modern forwarding, so a backend that cannot verify the signed
	// handshake rejects every login the proxy sends it; only an image whose
	// entrypoint consumes FELIS_FORWARDING_SECRET is actually reachable by a
	// player (see operator.buildEnv, which injects it into every backend but
	// cannot make an operator-typed Dockerfile read it). Recommending an arbitrary
	// public Minecraft image would therefore ship a trap: it builds, schedules,
	// and goes Ready, then refuses every join. Only the images Felis builds from
	// deploy/ clear that bar — see 0018_recommended_images.sql for which, and why
	// the honest set is one image rather than several.
	SourceRecommended = "recommended"
)

// ErrNotFound is returned when a build id / image ref does not exist.
var ErrNotFound = errors.New("build: not found")

// ErrAlreadyTerminal is returned by Cancel when the build has already finished.
var ErrAlreadyTerminal = errors.New("build: already in a terminal state")

// ErrInvalid wraps every request-validation failure (bad image ref, missing /
// oversize Dockerfile, missing context). Callers map it to a 400; it is kept
// distinct from store/cluster failures so those surface as 500.
var ErrInvalid = errors.New("build: invalid request")

// Request is the validated POST /images/build input (spec §16). The dockerfile
// and context are archived for audit; the target ref must address the internal
// registry (enforced in Validate).
type Request struct {
	// ImageRef is the push target, e.g. registry.felis.svc:5000/foo:1.0. It must
	// be under the configured internal registry — a build can never push
	// elsewhere.
	ImageRef string
	// Dockerfile is the uploaded build recipe (size-capped).
	Dockerfile string
	// ContextRef locates the uploaded tar.gz context in object storage / a PVC
	// (spec §17: Kaniko pulls it; Git context is intentionally not supported).
	ContextRef string
	// BaseImage is the resolved FROM, recorded for audit only — it is NOT a hard
	// gate (spec §16: base FROM is not hard-gated; the scan + egress lock cover
	// poisoned bases).
	BaseImage string
	// RequestedBy is the admin Access email, used as the audit actor and the
	// added_by of any admitted image.
	RequestedBy string
}

// Build mirrors an image_builds row (spec §6).
type Build struct {
	ID          string     `json:"id"`
	ImageRef    string     `json:"image_ref"`
	Status      Status     `json:"status"`
	Dockerfile  string     `json:"dockerfile,omitempty"`
	ContextRef  string     `json:"context_ref,omitempty"`
	BaseImage   string     `json:"base_image,omitempty"`
	RequestedBy string     `json:"requested_by"`
	JobName     string     `json:"job_name,omitempty"`
	LogRef      string     `json:"log_ref,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Image mirrors an image_whitelist row (spec §6): the dynamic, auditable image
// admission list that the create-server form reads from.
type Image struct {
	ImageRef string    `json:"image_ref"`
	Source   string    `json:"source"`
	BuildID  string    `json:"build_id,omitempty"`
	AddedBy  string    `json:"added_by"`
	Enabled  bool      `json:"enabled"`
	AddedAt  time.Time `json:"added_at"`
}

// Store is the business-layer persistence the Builder depends on (image_builds
// + image_whitelist). It is an interface so the Builder is tested against an
// in-memory fake; the Postgres implementation (pgStore) is integration-tested
// only.
type Store interface {
	// CreateBuild inserts a new image_builds row (status pending).
	CreateBuild(ctx context.Context, b *Build) error
	// GetBuild loads one build, or ErrNotFound.
	GetBuild(ctx context.Context, id string) (*Build, error)
	// SetBuildJob records the Job name and advances status to building.
	SetBuildJob(ctx context.Context, id, jobName string) error
	// FinishBuild sets a terminal status, an optional error, and finished_at.
	FinishBuild(ctx context.Context, id string, status Status, errMsg string, at time.Time) error
	// ListUnfinishedBuilds returns builds still being reconciled (status pending
	// or building), oldest first — the work list for SyncAll.
	ListUnfinishedBuilds(ctx context.Context) ([]Build, error)
	// AdmitBuiltImage upserts an image_whitelist row with enabled=true and
	// source=built (the scan-gate success path, spec §16). It records added_by.
	AdmitBuiltImage(ctx context.Context, img Image) error
	// ListImages returns the image whitelist.
	ListImages(ctx context.Context) ([]Image, error)
	// AddExternalImage upserts an externally-pushed image (spec §15 external
	// admission; source=external, no build_id).
	AddExternalImage(ctx context.Context, img Image) error
	// RemoveImage deletes an image_whitelist row, or ErrNotFound.
	RemoveImage(ctx context.Context, imageRef string) error
}

// Jobs is the cluster-side build lifecycle the Builder depends on. It is an
// interface so the scan-gate translation is tested against a fake; the
// controller-runtime implementation (k8sJobs) is integration-tested only — it
// requires a live cluster.
type Jobs interface {
	// CreateBuildJob starts the Kaniko+Trivy Job for p in the felis-build
	// namespace and returns the Job name.
	CreateBuildJob(ctx context.Context, p JobParams) (jobName string, err error)
	// JobPhase reports the current phase of a previously-created Job.
	JobPhase(ctx context.Context, jobName string) (JobPhase, error)
	// CancelBuildJob deletes the Job (and its pods), tolerating not-found.
	CancelBuildJob(ctx context.Context, jobName string) error
}

// Config parameterises the build subsystem from felis.toml (spec §24 [registry]
// + safety limits). It is validated by withDefaults before use.
type Config struct {
	// Namespace is the isolated build namespace (spec §16: felis-build).
	Namespace string
	// ServiceAccount is the weak SA the build Pod runs as. It MUST NOT be the
	// felis-api SA (spec §16 red line).
	ServiceAccount string
	// RegistryURL is the internal registry the build pushes to and Trivy scans
	// (spec §17). Image refs are validated to be under it.
	RegistryURL string
	// FelisImage is the platform image whose `fetch-context` entrypoint streams a
	// submission's context from the internal face into the build Pod. Required
	// only when a build's ContextRef is an http(s) URL (the submit lane's derived
	// shape); an install that never builds user submissions can leave it empty.
	FelisImage string
	// KanikoImage / TrivyImage are the executor images.
	KanikoImage string
	TrivyImage  string
	// Deadline caps a build's wall-clock (spec §16: activeDeadlineSeconds).
	Deadline time.Duration
	// MaxDockerfileBytes caps the uploaded Dockerfile (spec §16: context size
	// limits). Zero applies the default.
	MaxDockerfileBytes int
	// CPULimit / MemLimit cap each build container (spec §16: resource limits).
	CPULimit string
	MemLimit string
}

// Defaults applied when a Config field is left zero.
const (
	defaultNamespace      = "felis-build"
	defaultServiceAccount = "felis-build"
	defaultKanikoImage    = "gcr.io/kaniko-project/executor:latest"
	defaultTrivyImage     = "aquasec/trivy:latest"
	defaultDeadline       = 30 * time.Minute
	defaultMaxDockerfile  = 256 * 1024 // 256 KiB
	defaultCPULimit       = "2"
	defaultMemLimit       = "4Gi"
)

// withDefaults returns a copy of c with zero fields filled, so a partially
// configured Config (or the zero value, in tests) is always usable.
func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = defaultServiceAccount
	}
	if c.KanikoImage == "" {
		c.KanikoImage = defaultKanikoImage
	}
	if c.TrivyImage == "" {
		c.TrivyImage = defaultTrivyImage
	}
	if c.Deadline <= 0 {
		c.Deadline = defaultDeadline
	}
	if c.MaxDockerfileBytes <= 0 {
		c.MaxDockerfileBytes = defaultMaxDockerfile
	}
	if c.CPULimit == "" {
		c.CPULimit = defaultCPULimit
	}
	if c.MemLimit == "" {
		c.MemLimit = defaultMemLimit
	}
	return c
}

// Builder orchestrates the build subsystem. It holds no mutable state; the
// clock and id generator are injectable for hermetic tests.
type Builder struct {
	Store  Store
	Jobs   Jobs
	Config Config

	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
	// IDGen mints build ids. Defaults to a time-based generator.
	IDGen func() string
}

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Builder) newID() string {
	if b.IDGen != nil {
		return b.IDGen()
	}
	return fmt.Sprintf("bld-%d", time.Now().UnixNano())
}

// Submit validates req, records a pending build, and starts the Kaniko+Trivy
// Job (spec §16). The build is returned in the building state once the Job is
// created; if Job creation fails the build is marked failed so it never lingers
// pending. The caller (felis-api) drives the build to a terminal state by
// polling Sync / SyncAll.
func (b *Builder) Submit(ctx context.Context, req Request) (*Build, error) {
	cfg := b.Config.withDefaults()
	if err := Validate(req, cfg); err != nil {
		return nil, err
	}

	now := b.now()
	bld := &Build{
		ID:          b.newID(),
		ImageRef:    req.ImageRef,
		Status:      StatusPending,
		Dockerfile:  req.Dockerfile,
		ContextRef:  req.ContextRef,
		BaseImage:   req.BaseImage,
		RequestedBy: req.RequestedBy,
		CreatedAt:   now,
	}
	if err := b.Store.CreateBuild(ctx, bld); err != nil {
		return nil, err
	}

	jobName, err := b.Jobs.CreateBuildJob(ctx, b.jobParams(bld, cfg))
	if err != nil {
		// The pending row exists; mark it failed so it is not reconciled forever.
		_ = b.Store.FinishBuild(ctx, bld.ID, StatusFailed, "job creation failed: "+err.Error(), b.now())
		// felis_image_build_failures_total (spec §23): this terminal-failure path
		// records the build directly, not via finishAt, so it increments the counter
		// itself. The FinishBuild error is deliberately ignored (the build is failed
		// for the caller regardless), so the count tracks the failure event, not the
		// store write.
		metrics.ImageBuildFailuresTotal.Inc()
		bld.Status = StatusFailed
		bld.Error = "job creation failed: " + err.Error()
		return bld, fmt.Errorf("build: create job: %w", err)
	}

	if err := b.Store.SetBuildJob(ctx, bld.ID, jobName); err != nil {
		return nil, err
	}
	bld.JobName = jobName
	bld.Status = StatusBuilding
	return bld, nil
}

// jobParams projects a build + config onto the inputs jobspec.go renders.
func (b *Builder) jobParams(bld *Build, cfg Config) JobParams {
	return JobParams{
		BuildID:        bld.ID,
		ImageRef:       bld.ImageRef,
		ContextRef:     bld.ContextRef,
		Namespace:      cfg.Namespace,
		ServiceAccount: cfg.ServiceAccount,
		RegistryURL:    cfg.RegistryURL,
		FelisImage:     cfg.FelisImage,
		KanikoImage:    cfg.KanikoImage,
		TrivyImage:     cfg.TrivyImage,
		Deadline:       cfg.Deadline,
		CPULimit:       cfg.CPULimit,
		MemLimit:       cfg.MemLimit,
	}
}

// Get returns a build by id, or ErrNotFound.
func (b *Builder) Get(ctx context.Context, id string) (*Build, error) {
	return b.Store.GetBuild(ctx, id)
}

// Sync reconciles one non-terminal build against its Job phase — the scan-gate
// translation (spec §16). A terminal build is returned unchanged (idempotent).
//
//   - JobSucceeded → status=succeeded AND the image is admitted to the whitelist
//     with enabled=true (kaniko pushed and trivy found no CRITICAL CVE).
//   - JobFailed / JobUnknown → status=failed, nothing admitted (a CRITICAL CVE
//     surfaces here as a failed Job, since trivy runs with --exit-code 1).
//   - JobPending / JobRunning → no change.
//
// The image admission is performed by felis-api (this code path), never by the
// build Pod, which holds no database credentials.
func (b *Builder) Sync(ctx context.Context, id string) (*Build, error) {
	bld, err := b.Store.GetBuild(ctx, id)
	if err != nil {
		return nil, err
	}
	if bld.Status.terminal() {
		return bld, nil
	}
	if bld.JobName == "" {
		// Created but the Job name was never recorded; treat as failed rather
		// than reconcile forever against a phantom Job.
		return b.finish(ctx, bld, StatusFailed, "no build job recorded")
	}

	phase, err := b.Jobs.JobPhase(ctx, bld.JobName)
	if err != nil {
		return nil, err
	}
	switch phase {
	case JobSucceeded:
		now := b.now()
		// Admit the image first; only then mark the build succeeded, so a
		// succeeded build always has its whitelist row (no admitted-but-not-
		// recorded window if the second write fails).
		if err := b.Store.AdmitBuiltImage(ctx, Image{
			ImageRef: bld.ImageRef,
			Source:   SourceBuilt,
			BuildID:  bld.ID,
			AddedBy:  bld.RequestedBy,
			Enabled:  true,
			AddedAt:  now,
		}); err != nil {
			return nil, err
		}
		return b.finishAt(ctx, bld, StatusSucceeded, "", now)
	case JobFailed, JobUnknown:
		return b.finish(ctx, bld, StatusFailed, "build job failed or scan found a CRITICAL CVE")
	default: // JobPending / JobRunning
		return bld, nil
	}
}

// SyncAll reconciles every unfinished build and returns the count advanced to a
// terminal state. felis-api calls this periodically (spec §16: the scan gate is
// observed, not pushed by the build Pod).
func (b *Builder) SyncAll(ctx context.Context) (int, error) {
	builds, err := b.Store.ListUnfinishedBuilds(ctx)
	if err != nil {
		return 0, err
	}
	advanced := 0
	for i := range builds {
		bld, err := b.Sync(ctx, builds[i].ID)
		if err != nil {
			return advanced, err
		}
		if bld.Status.terminal() {
			advanced++
		}
	}
	return advanced, nil
}

// Cancel stops an in-flight build: delete its Job and mark it cancelled. A
// build that has already finished returns ErrAlreadyTerminal.
func (b *Builder) Cancel(ctx context.Context, id string) (*Build, error) {
	bld, err := b.Store.GetBuild(ctx, id)
	if err != nil {
		return nil, err
	}
	if bld.Status.terminal() {
		return nil, ErrAlreadyTerminal
	}
	if bld.JobName != "" {
		if err := b.Jobs.CancelBuildJob(ctx, bld.JobName); err != nil {
			return nil, err
		}
	}
	return b.finish(ctx, bld, StatusCancelled, "cancelled by administrator")
}

// finish marks a build terminal at the current clock and returns the updated
// view without a second round-trip.
func (b *Builder) finish(ctx context.Context, bld *Build, status Status, msg string) (*Build, error) {
	return b.finishAt(ctx, bld, status, msg, b.now())
}

func (b *Builder) finishAt(ctx context.Context, bld *Build, status Status, msg string, at time.Time) (*Build, error) {
	if err := b.Store.FinishBuild(ctx, bld.ID, status, msg, at); err != nil {
		return nil, err
	}
	if status == StatusFailed {
		// felis_image_build_failures_total (spec §23) counts builds that reached a
		// failed terminal state — a kaniko failure or a CRITICAL CVE surfaced by
		// trivy's --exit-code 1, observed here as the Sync JobFailed/JobUnknown
		// verdict. Cancellations (StatusCancelled) are deliberately not failures.
		// Incremented only after the failed status is persisted, so the counter
		// never runs ahead of the store. (Submit's job-creation path records its
		// failure outside finishAt and increments there.)
		metrics.ImageBuildFailuresTotal.Inc()
	}
	bld.Status = status
	bld.Error = msg
	finished := at
	bld.FinishedAt = &finished
	return bld, nil
}

// ListImages returns the image whitelist (spec §15 create-server form source).
func (b *Builder) ListImages(ctx context.Context) ([]Image, error) {
	return b.Store.ListImages(ctx)
}

// AddExternalImage admits an externally-pushed image (spec §15). It is enabled
// immediately; external images bypass the build pipeline but are still recorded
// with added_by for audit.
func (b *Builder) AddExternalImage(ctx context.Context, imageRef, addedBy string) (*Image, error) {
	if err := ValidateImageRef(imageRef); err != nil {
		return nil, err
	}
	img := Image{
		ImageRef: imageRef,
		Source:   SourceExternal,
		AddedBy:  addedBy,
		Enabled:  true,
		AddedAt:  b.now(),
	}
	if err := b.Store.AddExternalImage(ctx, img); err != nil {
		return nil, err
	}
	return &img, nil
}

// RemoveImage withdraws an image from the whitelist (spec §22: dynamic,
// auditable). It does not delete the underlying registry blob.
func (b *Builder) RemoveImage(ctx context.Context, imageRef string) error {
	return b.Store.RemoveImage(ctx, imageRef)
}

// ImageAdmitted reports whether a concrete image reference is on the whitelist
// and enabled (spec §15: the create-server form may only choose an admitted
// image). A disabled row never admits. A wildcard whitelist entry
// ("registry/foo:*") admits any concrete tag on that repo (imageMatches); the
// caller always passes a concrete ref, never a wildcard. An empty ref is never
// admitted.
func (b *Builder) ImageAdmitted(ctx context.Context, imageRef string) (bool, error) {
	if strings.TrimSpace(imageRef) == "" {
		return false, nil
	}
	images, err := b.Store.ListImages(ctx)
	if err != nil {
		return false, err
	}
	for _, img := range images {
		if img.Enabled && imageMatches(imageRef, img.ImageRef) {
			return true, nil
		}
	}
	return false, nil
}
