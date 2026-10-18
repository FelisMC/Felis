// Package build runs isolated build/scan Jobs; only the API records results and admits images in the database.
package build

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"felis.lolicon.best/internal/metrics"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusBuilding  Status = "building"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

func (s Status) terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// JobPhase is the build Pod's lifecycle as observed from the K8s Job.
type JobPhase int

const (
	JobUnknown JobPhase = iota // Job not found (e.g. garbage-collected); treated as failed
	JobPending
	JobRunning
	JobSucceeded // scan gate passed and the image was pushed
	JobFailed    // kaniko, the scan gate or the push failed; nothing is admitted
)

// Values of image_whitelist.source.
const (
	SourceBuilt    = "built"
	SourceExternal = "external"
	// Recommended is display metadata only; enabled controls admission and forwarding support controls joinability.
	SourceRecommended = "recommended"
)

var ErrNotFound = errors.New("build: not found")

var ErrAlreadyTerminal = errors.New("build: already in a terminal state")

// ErrInvalid wraps request-validation failures; callers map it to 400.
var ErrInvalid = errors.New("build: invalid request")

type Request struct {
	ImageRef   string // push target; must be under the configured internal registry
	Dockerfile string
	ContextRef string // tar.gz context location, or an http(s) URL on the internal API face
	// ContextDigest is the lowercase hex sha256 the fetch step enforces; requires an http(s) ContextRef.
	ContextDigest string
	BaseImage     string // base image after FROM, audit only
	RequestedBy   string // admin email: audit actor and added_by of an admitted image
}

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

	ContextDigest string `json:"context_digest,omitempty"` // Request.ContextDigest: the bytes the build was allowed to consume
}

type Image struct {
	ImageRef string    `json:"image_ref"`
	Source   string    `json:"source"`
	BuildID  string    `json:"build_id,omitempty"`
	AddedBy  string    `json:"added_by"`
	Enabled  bool      `json:"enabled"`
	AddedAt  time.Time `json:"added_at"`
}

// ListOpts selects a page of the build history. Query matches a build id or status exactly,
// or any part of the image ref, ignoring case; empty matches all.
type ListOpts struct {
	Query  string
	Limit  int
	Offset int
}

const (
	DefaultListLimit = 20
	MaxListLimit     = 100
)

// Store is the persistence the Builder depends on (image_builds and image_whitelist).
type Store interface {
	CreateBuild(ctx context.Context, b *Build) error
	// GetBuild returns ErrNotFound for an unknown id.
	GetBuild(ctx context.Context, id string) (*Build, error)
	// GetBuilds returns the builds among ids that exist, unordered and without their Dockerfile.
	GetBuilds(ctx context.Context, ids []string) ([]Build, error)
	SetBuildJob(ctx context.Context, id, jobName string) error
	FinishBuild(ctx context.Context, id string, status Status, errMsg string, at time.Time) error
	// ListUnfinishedBuilds returns pending and building builds, oldest first.
	ListUnfinishedBuilds(ctx context.Context) ([]Build, error)
	// ListBuilds returns one page, newest first, and the total match count; rows omit the Dockerfile.
	ListBuilds(ctx context.Context, opts ListOpts) ([]Build, int, error)
	// AdmitBuiltImage upserts an enabled source=built whitelist row.
	AdmitBuiltImage(ctx context.Context, img Image) error
	// SaveScan replaces any earlier scan record of the build.
	SaveScan(ctx context.Context, s Scan) error
	// GetScan returns ErrNotFound when the build has no scan.
	GetScan(ctx context.Context, buildID string) (*Scan, error)
	ListImages(ctx context.Context) ([]Image, error)
	// AddExternalImage upserts a source=external row with no build_id.
	AddExternalImage(ctx context.Context, img Image) error
	// RemoveImage returns ErrNotFound for an unknown ref.
	RemoveImage(ctx context.Context, imageRef string) error
}

type Jobs interface {
	CreateBuildJob(ctx context.Context, p JobParams) (jobName string, err error)
	JobPhase(ctx context.Context, jobName string) (JobPhase, error)
	// CancelBuildJob deletes the Job and its pods, tolerating not-found.
	CancelBuildJob(ctx context.Context, jobName string) error
}

// JobOutcomes reads what a finished build pod left behind; a nil JobOutcomes keeps Sync to the Job phase alone.
type JobOutcomes interface {
	Outcome(ctx context.Context, buildID string) (Outcome, error)
}

type Outcome struct {
	FailedStep       string // container whose non-zero exit ended the pod; "" when none did
	ExitCode         int32
	Message          string // termination message
	DeadlineExceeded bool   // the Job ran past activeDeadlineSeconds
	// Scan is nil when scan-gate never ran or its log held no envelope; ScanErr then says why.
	Scan    *ScanEnvelope
	ScanErr error
}

type Config struct {
	Namespace string // isolated build namespace
	// ServiceAccount is the weak SA the build Pod runs as; it must not be the felis-api SA.
	ServiceAccount string
	// RegistryURL is the internal registry builds push to; image refs must be under it.
	RegistryURL string
	// FelisImage provides the fetch-context entrypoint; needed only for http(s) ContextRefs.
	FelisImage string
	// TrivyDBRepository overrides --db-repository; empty uses the platform registry's copy when RegistryURL is set.
	TrivyDBRepository string
	// TrivyJavaDBRepository overrides --java-db-repository and defaults like TrivyDBRepository.
	TrivyJavaDBRepository string
	// KanikoImage and TrivyImage default to the platform registry's copies (Tools).
	KanikoImage string
	TrivyImage  string
	// ScanFailOn lists blocking severities (empty applies DefaultScanFailOn); see ScanPolicy.
	ScanFailOn      []string
	ScanFailUnfixed bool
	ScanAccept      []string      // finding ids that never block
	Deadline        time.Duration // activeDeadlineSeconds
	// MaxDockerfileBytes caps the uploaded Dockerfile; zero applies the default.
	MaxDockerfileBytes int
	CPULimit           string // per build container
	MemLimit           string // per build container
	// DiskLimit caps ephemeral storage: extracted context, unpacked base image and image tarball together.
	DiskLimit string
	// UserNamespaces is UserNamespacesOn, UserNamespacesOff or UserNamespacesAuto (default, follows UserNamespacesProbe).
	UserNamespaces string
	// UserNamespacesProbe carries ProbeUserNamespaces' verdict to every copy of this Config; nil keeps "auto" off.
	UserNamespacesProbe *atomic.Bool
	RuntimeClass        string

	// MaxConcurrent caps running build Jobs; excess builds stay pending until SyncAll starts them. Bounded by MaxConcurrentLimit.
	MaxConcurrent int

	// Send context tokens only to ContextOrigin; an empty origin rejects all HTTP contexts.
	ContextOrigin string
}

const (
	UserNamespacesAuto = "auto"
	UserNamespacesOn   = "on"
	UserNamespacesOff  = "off"
)

func (c Config) userNamespaces() bool {
	switch c.UserNamespaces {
	case UserNamespacesOn:
		return true
	case UserNamespacesOff:
		return false
	}
	return c.UserNamespacesProbe != nil && c.UserNamespacesProbe.Load()
}

const (
	defaultNamespace      = "felis-build"
	defaultServiceAccount = "felis-build"
	defaultDeadline       = 30 * time.Minute
	defaultMaxDockerfile  = 256 * 1024 // 256 KiB
	defaultCPULimit       = "2"
	defaultMemLimit       = "4Gi"
	defaultDiskLimit      = "12Gi"
	defaultMaxConcurrent  = 2
)

// MaxConcurrentLimit is the highest MaxConcurrent accepted; BuildResourceQuota's pod quota fits this many build pods plus the user-namespace probe.
const MaxConcurrentLimit = 6

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = defaultServiceAccount
	}
	if c.KanikoImage == "" {
		c.KanikoImage = toolRef(c.RegistryURL, "kaniko")
	}
	if c.TrivyImage == "" {
		c.TrivyImage = toolRef(c.RegistryURL, "trivy")
	}
	if c.TrivyDBRepository == "" && c.RegistryURL != "" {
		c.TrivyDBRepository = toolRef(c.RegistryURL, "trivy-db")
	}
	if c.TrivyJavaDBRepository == "" && c.RegistryURL != "" {
		c.TrivyJavaDBRepository = toolRef(c.RegistryURL, "trivy-java-db")
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
	if c.DiskLimit == "" {
		c.DiskLimit = defaultDiskLimit
	}
	if c.UserNamespaces == "" {
		c.UserNamespaces = UserNamespacesAuto
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	if c.MaxConcurrent > MaxConcurrentLimit {
		c.MaxConcurrent = MaxConcurrentLimit
	}
	if len(c.ScanFailOn) == 0 {
		c.ScanFailOn = DefaultScanFailOn
	}
	return c
}

// Builder orchestrates builds. Its only state is startMu, which serializes Job starts so two submissions cannot take the last free slot.
type Builder struct {
	Store  Store
	Jobs   Jobs
	Config Config
	// Outcomes reads a finished pod's failed step and scan envelope; nil records a generic failure and no scan.
	Outcomes JobOutcomes

	startMu sync.Mutex

	Now   func() time.Time // defaults to time.Now
	IDGen func() string    // defaults to a time-based id
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

// Queue builds beyond concurrency limits; failed Job creation marks them failed.
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
	bld.ContextDigest = req.ContextDigest
	if err := b.Store.CreateBuild(ctx, bld); err != nil {
		return nil, err
	}

	b.startMu.Lock()
	defer b.startMu.Unlock()
	running, err := b.running(ctx)
	if err != nil || running >= cfg.MaxConcurrent {
		return bld, nil // queued; SyncAll retries the start
	}
	return b.start(ctx, bld, cfg)
}

func (b *Builder) running(ctx context.Context) (int, error) {
	builds, err := b.Store.ListUnfinishedBuilds(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range builds {
		if builds[i].Status == StatusBuilding {
			n++
		}
	}
	return n, nil
}

// start requires the caller to hold startMu.
func (b *Builder) start(ctx context.Context, bld *Build, cfg Config) (*Build, error) {
	jobName, err := b.Jobs.CreateBuildJob(ctx, b.jobParams(bld, cfg))
	if err != nil {
		_ = b.Store.FinishBuild(ctx, bld.ID, StatusFailed, "job creation failed: "+err.Error(), b.now())
		// Counted here because this path bypasses finishAt; the FinishBuild error is ignored.
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

func (b *Builder) jobParams(bld *Build, cfg Config) JobParams {
	return JobParams{
		BuildID:               bld.ID,
		ImageRef:              bld.ImageRef,
		ContextRef:            bld.ContextRef,
		ContextDigest:         bld.ContextDigest,
		Namespace:             cfg.Namespace,
		ServiceAccount:        cfg.ServiceAccount,
		RegistryURL:           cfg.RegistryURL,
		FelisImage:            cfg.FelisImage,
		TrivyDBRepository:     cfg.TrivyDBRepository,
		TrivyJavaDBRepository: cfg.TrivyJavaDBRepository,
		KanikoImage:           cfg.KanikoImage,
		TrivyImage:            cfg.TrivyImage,
		ScanFailOn:            cfg.ScanFailOn,
		ScanFailUnfixed:       cfg.ScanFailUnfixed,
		ScanAccept:            cfg.ScanAccept,
		Deadline:              cfg.Deadline,
		CPULimit:              cfg.CPULimit,
		MemLimit:              cfg.MemLimit,
		DiskLimit:             cfg.DiskLimit,
		UserNamespaces:        cfg.userNamespaces(),
		RuntimeClass:          cfg.RuntimeClass,
	}
}

func (b *Builder) Get(ctx context.Context, id string) (*Build, error) {
	return b.Store.GetBuild(ctx, id)
}

// GetMany returns the builds among ids that exist, keyed by id, as stored and without their Dockerfile.
func (b *Builder) GetMany(ctx context.Context, ids []string) (map[string]Build, error) {
	out := make(map[string]Build, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	builds, err := b.Store.GetBuilds(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, bld := range builds {
		out[bld.ID] = bld
	}
	return out, nil
}

func (b *Builder) ListBuilds(ctx context.Context, opts ListOpts) ([]Build, int, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	if opts.Limit <= 0 {
		opts.Limit = DefaultListLimit
	}
	opts.Limit = min(opts.Limit, MaxListLimit)
	opts.Offset = max(opts.Offset, 0)
	return b.Store.ListBuilds(ctx, opts)
}

// Store scan results before settling terminal Jobs; success admits the image and failures retain their reasons.
func (b *Builder) Sync(ctx context.Context, id string) (*Build, error) {
	bld, err := b.Store.GetBuild(ctx, id)
	if err != nil {
		return nil, err
	}
	if bld.Status.terminal() {
		return bld, nil
	}
	if bld.JobName == "" {
		if bld.Status == StatusPending {
			return bld, nil // queued; SyncAll starts it
		}
		return b.finish(ctx, bld, StatusFailed, "no build job recorded")
	}

	phase, err := b.Jobs.JobPhase(ctx, bld.JobName)
	if err != nil {
		return nil, err
	}
	if phase != JobSucceeded && phase != JobFailed && phase != JobUnknown {
		return bld, nil // JobPending / JobRunning
	}
	var out Outcome
	if b.Outcomes != nil && phase != JobUnknown {
		if out, err = b.Outcomes.Outcome(ctx, bld.ID); err != nil {
			return nil, err
		}
	}
	if out.Scan != nil {
		scan, err := newScan(bld.ID, out.Scan, b.now())
		if err != nil {
			return nil, err
		}
		if err := b.Store.SaveScan(ctx, scan); err != nil {
			return nil, err
		}
	}
	switch phase {
	case JobSucceeded:
		now := b.now()
		// Admit before marking succeeded so a succeeded build always has its whitelist row.
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
	case JobUnknown:
		return b.finish(ctx, bld, StatusFailed, "the build job is gone: it was deleted before it finished")
	default:
		return b.finish(ctx, bld, StatusFailed, b.failureReason(out))
	}
}

var stepNames = map[string]string{
	ContainerGate:     "the egress gate",
	ContainerFetch:    "fetching the build context",
	ContainerKaniko:   "the image build",
	ContainerTrivy:    "the vulnerability scan",
	ContainerSBOM:     "writing the SBOM",
	ContainerScanGate: "the scan gate",
	ContainerPush:     "the registry push",
}

const maxFailureMessage = 600 // bytes

// failureReason is the error a failed build records: the scan verdict, the deadline, or the failed step with its message tail.
func (b *Builder) failureReason(out Outcome) string {
	switch {
	case out.Scan != nil && out.Scan.Summary.Blocked:
		return out.Scan.Summary.Reason()
	case out.DeadlineExceeded:
		return fmt.Sprintf("the build ran past its %s deadline", b.Config.withDefaults().Deadline)
	case out.FailedStep == "":
		return "the build job failed"
	}
	name := stepNames[out.FailedStep]
	if name == "" {
		name = "the " + out.FailedStep + " step"
	}
	msg := fmt.Sprintf("%s failed (exit %d)", name, out.ExitCode)
	if tail := messageTail(out.Message); tail != "" {
		msg += ": " + tail
	}
	if out.FailedStep == ContainerScanGate && out.ScanErr != nil {
		msg += "; the scan report could not be read back: " + out.ScanErr.Error()
	}
	return msg
}

// messageTail joins the last three non-empty lines of m on one line with control characters removed, capped at maxFailureMessage bytes.
func messageTail(m string) string {
	var lines []string
	for _, l := range strings.Split(m, "\n") {
		if l = strings.TrimSpace(Printable(strings.ReplaceAll(strings.TrimRight(l, "\r"), "\t", " "))); l != "" {
			lines = append(lines, l)
		}
	}
	lines = lines[max(0, len(lines)-3):]
	out := strings.Join(lines, " | ")
	if len(out) > maxFailureMessage {
		out = "…" + strings.ToValidUTF8(out[len(out)-maxFailureMessage:], "")
	}
	return out
}

func (b *Builder) Scan(ctx context.Context, id string) (*Scan, error) {
	return b.Store.GetScan(ctx, id)
}

// Reconcile running builds, then start queued builds oldest first; collect errors without blocking other builds.
func (b *Builder) SyncAll(ctx context.Context) (int, error) {
	builds, err := b.Store.ListUnfinishedBuilds(ctx)
	if err != nil {
		return 0, err
	}
	advanced := 0
	var errs []error
	for i := range builds {
		if builds[i].Status == StatusPending && builds[i].JobName == "" {
			continue // queued; startQueued below
		}
		bld, err := b.Sync(ctx, builds[i].ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("build %s: %w", builds[i].ID, err))
			continue
		}
		if bld.Status.terminal() {
			advanced++
		}
	}
	started, err := b.startQueued(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	advanced += started
	return advanced, errors.Join(errs...)
}

// startQueued starts pending builds oldest first while fewer than MaxConcurrent run.
// It returns how many failed outright (Job creation error), which count as advanced.
func (b *Builder) startQueued(ctx context.Context) (int, error) {
	cfg := b.Config.withDefaults()
	b.startMu.Lock()
	defer b.startMu.Unlock()
	builds, err := b.Store.ListUnfinishedBuilds(ctx)
	if err != nil {
		return 0, err
	}
	running := 0
	for i := range builds {
		if builds[i].Status == StatusBuilding {
			running++
		}
	}
	failed := 0
	var errs []error
	for i := range builds {
		if running >= cfg.MaxConcurrent {
			break
		}
		bld := &builds[i]
		if bld.Status != StatusPending || bld.JobName != "" {
			continue
		}
		if _, err := b.start(ctx, bld, cfg); err != nil {
			errs = append(errs, fmt.Errorf("build %s: %w", bld.ID, err))
			if bld.Status == StatusFailed {
				failed++
			}
			continue
		}
		running++
	}
	return failed, errors.Join(errs...)
}

// Cancel deletes the build's Job and marks it cancelled; a finished build returns ErrAlreadyTerminal.
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

func (b *Builder) finish(ctx context.Context, bld *Build, status Status, msg string) (*Build, error) {
	return b.finishAt(ctx, bld, status, msg, b.now())
}

func (b *Builder) finishAt(ctx context.Context, bld *Build, status Status, msg string, at time.Time) (*Build, error) {
	if err := b.Store.FinishBuild(ctx, bld.ID, status, msg, at); err != nil {
		return nil, err
	}
	if status == StatusFailed {
		// Counted after the status is persisted; cancellations are not failures.
		metrics.ImageBuildFailuresTotal.Inc()
	}
	bld.Status = status
	bld.Error = msg
	finished := at
	bld.FinishedAt = &finished
	return bld, nil
}

func (b *Builder) ListImages(ctx context.Context) ([]Image, error) {
	return b.Store.ListImages(ctx)
}

// AddExternalImage admits an externally-pushed image, enabled immediately and recorded with added_by.
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

// RemoveImage withdraws an image from the whitelist without deleting its registry blob.
func (b *Builder) RemoveImage(ctx context.Context, imageRef string) error {
	return b.Store.RemoveImage(ctx, imageRef)
}

// Enabled wildcard tags admit matching references; digest pins are checked against their name:tag.
func (b *Builder) ImageAdmitted(ctx context.Context, imageRef string) (bool, error) {
	imageRef, _, _ = strings.Cut(imageRef, "@")
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
