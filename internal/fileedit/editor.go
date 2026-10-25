// Package fileedit executes world-volume operations in isolated Jobs and returns marked results through pod logs.
package fileedit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
)

var (
	// ErrNotFound refers to a path inside the world, not a missing server.
	ErrNotFound = errors.New("fileedit: no such file or directory")
	// ErrBadPath rejects root escapes or paths incompatible with the requested operation.
	ErrBadPath  = errors.New("fileedit: path is not accessible")
	ErrTooLarge = errors.New("fileedit: file is too large for the editor")
	ErrConflict = errors.New("fileedit: file changed since it was read")
	// ErrNoSpace leaves the original file unchanged.
	ErrNoSpace     = errors.New("fileedit: the world volume is full")
	ErrExists      = errors.New("fileedit: the target already exists")
	ErrReadDamaged = errors.New("fileedit: the file's bytes changed on their way from the file Job")
)

// Runner returns synchronous Job results and exposes long uploads or unzips through Start and Ops.
type Runner interface {
	// Run returns the JSON payload marked with ResultPrefix from the Pod log.
	Run(ctx context.Context, p JobParams) ([]byte, error)
	// Start returns once the Job exists, before completion.
	Start(ctx context.Context, p JobParams) error
	// Ops lists retained asynchronous Jobs newest first.
	Ops(ctx context.Context, namespace, server string) ([]OpState, error)
}

type Config struct {
	StageWrite   func(content []byte) (UploadSource, func(), error)
	ResolveWorld placement.Resolver
	Namespace    string
	// ServiceAccount needs filesystem access only and no Kubernetes RBAC grants.
	ServiceAccount string
	Image          string
	// WorldsRoot defaults to /data to match paths inside the game container.
	WorldsRoot string
	Deadline   time.Duration
	// Timeout bounds the caller's wait independently of the Job's longer Deadline.
	Timeout  time.Duration
	CPULimit string
	MemLimit string
	// Zero FSGroup is omitted to avoid changing ownership across the mounted game volume.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// TTLAfterFinished is the cleanup path because the API cannot delete Jobs; retain logs long enough for result collection.
	TTLAfterFinished time.Duration

	// AsyncDeadline, AsyncTTL and AsyncCPULimit override synchronous limits for uploads and unzips.
	AsyncDeadline time.Duration
	AsyncTTL      time.Duration
	AsyncCPULimit string
}

const (
	defaultNamespace      = "minecraft"
	defaultServiceAccount = "felis-restore"
	defaultWorldsRoot     = "/data"
	defaultDeadline       = 2 * time.Minute
	defaultTimeout        = 90 * time.Second
	defaultCPULimit       = "500m"
	defaultMemLimit       = "256Mi"
	defaultTTL            = 2 * time.Minute
	defaultAsyncDeadline  = 2 * time.Hour
	defaultAsyncTTL       = 30 * time.Minute
	defaultAsyncCPULimit  = "1"
)

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = defaultServiceAccount
	}
	if c.WorldsRoot == "" {
		c.WorldsRoot = defaultWorldsRoot
	}
	if c.Deadline <= 0 {
		c.Deadline = defaultDeadline
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.CPULimit == "" {
		c.CPULimit = defaultCPULimit
	}
	if c.MemLimit == "" {
		c.MemLimit = defaultMemLimit
	}
	if c.TTLAfterFinished <= 0 {
		c.TTLAfterFinished = defaultTTL
	}
	if c.AsyncDeadline <= 0 {
		c.AsyncDeadline = defaultAsyncDeadline
	}
	if c.AsyncTTL <= 0 {
		c.AsyncTTL = defaultAsyncTTL
	}
	if c.AsyncCPULimit == "" {
		c.AsyncCPULimit = defaultAsyncCPULimit
	}
	return c
}

type Editor struct {
	Runner Runner
	Config Config
}

type Listing struct {
	Entries []Entry
	// Truncated means entries were limited to MaxEntries.
	Truncated bool
	// Free is available bytes, negative when unknown.
	Free int64
}

func (e *Editor) List(ctx context.Context, server, path string) (Listing, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpList, Path: path})
	if err != nil {
		return Listing{}, err
	}
	if res.Entries == nil {
		res.Entries = []Entry{}
	}
	return Listing{Entries: res.Entries, Truncated: res.Truncated, Free: res.Avail}, nil
}

func (e *Editor) Search(ctx context.Context, server, path, query string) (Listing, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpSearch, Path: path, Query: query})
	if err != nil {
		return Listing{}, err
	}
	if res.Entries == nil {
		res.Entries = []Entry{}
	}
	return Listing{Entries: res.Entries, Truncated: res.Truncated, Free: -1}, nil
}

func (e *Editor) Extensions(ctx context.Context, server string) (ExtensionList, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpExtensions})
	if err != nil {
		return ExtensionList{}, err
	}
	if res.Extensions == nil {
		res.Extensions = []Extension{}
	}
	return ExtensionList{Entries: res.Extensions, Truncated: res.Truncated}, nil
}

func (e *Editor) DiskUsage(ctx context.Context, server string) (int64, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpDiskUsage})
	if err != nil {
		return 0, err
	}
	if res.UsedBytes == nil || *res.UsedBytes < 0 {
		return 0, fmt.Errorf("%w: the file Job returned no valid disk usage", ErrReadDamaged)
	}
	return *res.UsedBytes, nil
}

func (e *Editor) LogTail(ctx context.Context, server, cursor string) (LogTailPage, error) {
	if _, err := parseLogTailCursor(cursor); err != nil {
		return LogTailPage{}, fmt.Errorf("%w: invalid log cursor", ErrBadPath)
	}
	res, err := e.run(ctx, server, JobParams{Op: OpLogTail, Cursor: cursor})
	if err != nil {
		return LogTailPage{}, err
	}
	if res.LogTail == nil {
		return LogTailPage{}, fmt.Errorf("%w: the file Job returned no log page", ErrReadDamaged)
	}
	page := res.LogTail
	if got := digest(page.Content); got != page.ContentSHA256 {
		return LogTailPage{}, fmt.Errorf("%w: they hash to %s, sent as %q", ErrReadDamaged, got, page.ContentSHA256)
	}
	if page.Content == nil {
		page.Content = []byte{}
	}
	return *page, nil
}

// Read returns the pre-redaction file hash for conditional writes and rejects damaged transmitted content.
func (e *Editor) Read(ctx context.Context, server, path string) ([]byte, string, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpRead, Path: path})
	if err != nil {
		return nil, "", err
	}
	if got := digest(res.Content); got != res.ContentSHA256 {
		return nil, "", fmt.Errorf("%w: they hash to %s, sent as %q", ErrReadDamaged, got, res.ContentSHA256)
	}
	if res.Content == nil {
		res.Content = []byte{}
	}
	return res.Content, res.SHA256, nil
}

// Write creates missing files but not parents, compares nonempty expect hashes and rejects existing paths with createOnly.
func (e *Editor) Write(ctx context.Context, server, path string, content []byte, expect string, createOnly bool) (string, error) {
	if len(content) > MaxWriteBytes {
		return "", ErrTooLarge
	}
	p := JobParams{Op: OpWrite, Path: path, Content: content, Expect: expect, CreateOnly: createOnly}
	if len(content) > MaxInlineWriteBytes {
		if e.Config.StageWrite == nil {
			return "", fmt.Errorf("fileedit: large writes need staging")
		}
		src, cleanup, err := e.Config.StageWrite(content)
		if err != nil {
			return "", err
		}
		defer cleanup()
		p.Content = nil
		p.SourceURL, p.UploadToken, p.UploadSize, p.UploadSHA256 = src.URL, src.Token, src.Size, src.SHA256
	}
	res, err := e.run(ctx, server, p)
	if err != nil {
		return "", err
	}
	return res.SHA256, nil
}

func (e *Editor) Mkdir(ctx context.Context, server, path string) error {
	_, err := e.run(ctx, server, JobParams{Op: OpMkdir, Path: path})
	return err
}

func (e *Editor) Delete(ctx context.Context, server, path string) error {
	_, err := e.run(ctx, server, JobParams{Op: OpDelete, Path: path})
	return err
}

func (e *Editor) Rename(ctx context.Context, server, path, to string) error {
	_, err := e.run(ctx, server, JobParams{Op: OpRename, Path: path, To: to})
	return err
}

// UploadSource uses a single-use internal URL token and the exact size and digest required before landing.
type UploadSource struct {
	URL    string
	Token  string
	Size   int64
	SHA256 string
}

// Upload verifies src.Size and src.SHA256 before replacing a file; overwrite=false rejects existing targets.
func (e *Editor) Upload(ctx context.Context, server, path string, src UploadSource, overwrite bool) error {
	_, err := e.run(ctx, server, JobParams{
		Op: OpUpload, Path: path, Overwrite: overwrite,
		SourceURL: src.URL, UploadToken: src.Token, UploadSize: src.Size, UploadSHA256: src.SHA256,
	})
	return err
}

// Reject oversized writes before Job creation so etcd's object limit cannot turn a client error into an opaque backend failure.
func (e *Editor) run(ctx context.Context, server string, p JobParams) (Result, error) {
	if p.Op == OpWrite && len(p.Content) > MaxWriteBytes {
		return Result{}, fmt.Errorf("%w: content is %d bytes, the limit is %d",
			ErrTooLarge, len(p.Content), MaxWriteBytes)
	}

	cfg := e.Config.withDefaults()
	if p.Op == OpDiskUsage {
		cfg.Timeout = min(cfg.Timeout, diskUsageTimeout)
	}
	p, err := cfg.params(server, p)
	if err != nil {
		return Result{}, err
	}

	if cfg.ResolveWorld != nil {
		world, err := cfg.ResolveWorld(ctx, server)
		if err != nil {
			return Result{}, err
		}
		p.WorldPVC, p.NodeName = world.Claim, world.Node
	}

	// Bound the API wait independently; the Job deadline handles cluster cleanup after a caller timeout.
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	payload, err := e.Runner.Run(ctx, p)
	if err != nil {
		return Result{}, err
	}

	var res Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return Result{}, fmt.Errorf("fileedit: malformed result from the file Job: %w", err)
	}
	return res, resultError(res)
}

func (c Config) params(server string, p JobParams) (JobParams, error) {
	opID, err := newOpID()
	if err != nil {
		return JobParams{}, err
	}
	p.Server = server
	p.OpID = opID
	p.WorldPVC = naming.WorldPVCName(server)
	p.Namespace = c.Namespace
	p.ServiceAccount = c.ServiceAccount
	p.Image = c.Image
	p.WorldsRoot = c.WorldsRoot
	p.Deadline = c.Deadline
	p.CPULimit = c.CPULimit
	p.MemLimit = c.MemLimit
	p.RunAsUser = c.RunAsUser
	p.RunAsGroup = c.RunAsGroup
	p.FSGroup = c.FSGroup
	p.TTLAfterFinished = c.TTLAfterFinished
	return p, nil
}

const (
	OpRunning   = "running"
	OpSucceeded = "succeeded"
	OpFailed    = "failed"
)

type OpState struct {
	ID    string
	Op    string
	Path  string
	State string
	// Finished is zero while the Job runs.
	Started  time.Time
	Finished time.Time
	// Done and Total are bytes from the latest progress update, zero before the first.
	Done, Total int64
	// Result=nil means still running or no result was printed; Reason distinguishes terminated Jobs without results.
	Result *Result
	Reason string
}

func (e *Editor) StartUpload(ctx context.Context, server, path string, src UploadSource, overwrite bool) (OpState, error) {
	return e.start(ctx, server, JobParams{
		Op: OpUpload, Path: path, Overwrite: overwrite,
		SourceURL: src.URL, UploadToken: src.Token, UploadSize: src.Size, UploadSHA256: src.SHA256,
	})
}

// StartUnzip without overwrite reports conflicts without modifying existing files.
func (e *Editor) StartUnzip(ctx context.Context, server, path string, overwrite bool) (OpState, error) {
	return e.start(ctx, server, JobParams{Op: OpUnzip, Path: path, Overwrite: overwrite})
}

func (e *Editor) StartTransfer(ctx context.Context, server, op string, paths []string, to string, overwrite bool) (OpState, error) {
	if op != OpCopy && op != OpMove {
		return OpState{}, ErrBadPath
	}
	return e.start(ctx, server, JobParams{Op: op, Path: to, Paths: paths, To: to, Overwrite: overwrite})
}

func (e *Editor) StartZip(ctx context.Context, server string, paths []string, to string, overwrite bool) (OpState, error) {
	return e.start(ctx, server, JobParams{Op: OpZip, Path: to, Paths: paths, To: to, Overwrite: overwrite})
}

func (e *Editor) Ops(ctx context.Context, server string) ([]OpState, error) {
	return e.Runner.Ops(ctx, e.Config.withDefaults().Namespace, server)
}

func (e *Editor) start(ctx context.Context, server string, p JobParams) (OpState, error) {
	cfg := e.Config.withDefaults()
	p, err := cfg.params(server, p)
	if err != nil {
		return OpState{}, err
	}
	if cfg.ResolveWorld != nil {
		world, err := cfg.ResolveWorld(ctx, server)
		if err != nil {
			return OpState{}, err
		}
		p.WorldPVC, p.NodeName = world.Claim, world.Node
	}
	p.Async = true
	p.Deadline, p.TTLAfterFinished, p.CPULimit = cfg.AsyncDeadline, cfg.AsyncTTL, cfg.AsyncCPULimit
	if err := e.Runner.Start(ctx, p); err != nil {
		return OpState{}, err
	}
	return OpState{ID: p.OpID, Op: p.Op, Path: p.Path, State: OpRunning, Started: time.Now()}, nil
}

// resultError rejects unknown failure codes instead of treating them as success.
func resultError(res Result) error {
	switch res.Code {
	case "":
		return nil
	case CodeNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, res.Error)
	case CodeBadPath:
		return fmt.Errorf("%w: %s", ErrBadPath, res.Error)
	case CodeTooLarge:
		return fmt.Errorf("%w: %s", ErrTooLarge, res.Error)
	case CodeConflict, CodeRecovery:
		return fmt.Errorf("%w: %s", ErrConflict, res.Error)
	case CodeNoSpace:
		return fmt.Errorf("%w: %s", ErrNoSpace, res.Error)
	case CodeExists:
		return fmt.Errorf("%w: %s", ErrExists, res.Error)
	case CodeDigestMismatch:
		return fmt.Errorf("%w: %s", ErrDigestMismatch, res.Error)
	default:
		return fmt.Errorf("fileedit: file operation failed (%s): %s", res.Code, res.Error)
	}
}

func newOpID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fileedit: generate op id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
