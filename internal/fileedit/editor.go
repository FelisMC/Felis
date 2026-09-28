// Package fileedit implements the server file manager: list, read, write, make a
// folder, delete, rename and upload inside a server's world volume. It began as
// the lever an owner reaches for when a server will not boot because one line of
// server.properties or a plugin's YAML is wrong — the one repair that otherwise
// requires a human with cluster access — and also covers the everyday chores: drop
// in a plugin jar, clear out a folder, rename a world.
//
// felis-api cannot touch a world in-process: the world PVC is ReadWriteOnce and
// its lifecycle is owned by the operator's StatefulSet, so the API has nothing to
// mount at request time. That is the same constraint that makes internal/restore
// and internal/backupjob one-shot Jobs, and it has the same two consequences here:
// the work runs as a Job, and the server MUST be stopped first (a running server
// holds the RWO volume, so the Job could not mount it). The handlers enforce the
// stopped gate exactly as the backup/restore handlers do.
//
// # How a result gets back
//
// A file operation is unusual among Felis's Jobs in that the CALLER wants the
// output, not just the side effect: a listing and a file's bytes must reach the
// browser that asked. The transport is deliberately the narrowest one available —
// the Job PRINTS its result to stdout and felis-api reads it back through the
// pods/log subresource, which it already has RBAC for. This is the whole reason
// the design needs no new permission:
//
//	create the Job        → jobs:create           (already held)
//	find its Pod          → pods:list             (already held, for the §8 console)
//	read the result       → pods/log:get          (already held, for the §8 console)
//
// No pods/exec, no pods/portforward, not even pods:get — the least-privilege line
// internal/platform/rbac.go draws and a test asserts. The write direction travels
// the other way: an edit on the Job spec felis-api creates (see ContentEnv), an
// upload fetched by the Job from felis-api's internal face (see Stage), because a
// 64 MiB jar fits in neither a Job spec nor an environment.
//
// The price is latency: every operation is a Pod schedule + image pull, so a
// listing takes seconds rather than milliseconds. That is inherent to RWO plus a
// stopped server, not a property of this transport: the file manager works on a
// stopped server, one operation per Job.
//
// The Editor depends on the Runner interface, so the orchestration and the error
// mapping are unit-tested against an in-memory fake; the client-go implementation
// (k8sjobs.go) compiles here but is exercised only against a live cluster.
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
)

// Errors the Editor returns, which internal/api maps onto HTTP status codes
// (handlers_files.go). They are sentinels rather than an error type because the
// mapping needs nothing but identity — the human-readable detail rides along in
// the wrapped message.
var (
	// ErrNotFound is a path that resolves inside the world root but has nothing at
	// it. It is distinct from a missing SERVER, which the handler resolves earlier.
	ErrNotFound = errors.New("fileedit: no such file or directory")
	// ErrBadPath is a path the Job refused: it escapes the world root (via "..", an
	// absolute path, or a symlink), or names a directory where a file is required.
	ErrBadPath = errors.New("fileedit: path is not accessible")
	// ErrTooLarge is a read of a file over MaxReadBytes or a write over
	// MaxWriteBytes.
	ErrTooLarge = errors.New("fileedit: file is too large for the editor")
	// ErrConflict is a write whose expected hash no longer matches: the file
	// changed after the caller read it.
	ErrConflict = errors.New("fileedit: file changed since it was read")
	// ErrNoSpace is a write the world volume had no room for; the file is
	// unchanged.
	ErrNoSpace = errors.New("fileedit: the world volume is full")
	// ErrExists is a create, mkdir, rename or upload whose target is already
	// there.
	ErrExists = errors.New("fileedit: the target already exists")
)

// Runner is the cluster-side half of a file operation. Run renders and creates
// the Job, waits for its Pod to reach a terminal phase, and returns the marked
// JSON payload the Pod printed: from the API's point of view an operation is one
// call. An upload too big for one request and an unzip can outlast any request,
// so those two are started instead (Start) and read back later (Ops), from the
// Jobs felis-api lists and the progress lines their Pods print.
//
// It is an interface so the Editor's orchestration and error mapping are tested
// against a fake; the client-go implementation (K8sRunner) is integration-only.
type Runner interface {
	// Run creates the Job for p and returns the raw JSON payload from the
	// ResultPrefix line of its Pod's log.
	Run(ctx context.Context, p JobParams) ([]byte, error)
	// Start creates the Job for p and returns once it exists.
	Start(ctx context.Context, p JobParams) error
	// Ops reports the background operations (JobParams.Async) of one server
	// whose Jobs the cluster still holds, newest first.
	Ops(ctx context.Context, namespace, server string) ([]OpState, error)
}

// Config parameterises the file editor. Image has no default on purpose: it is
// deployment-specific, and when it is empty cmd/felis leaves the API's FileEditor
// nil so the endpoints report 503 rather than creating a Job that cannot run.
type Config struct {
	// Namespace is where the world PVCs live and the Job runs (the minecraft
	// namespace), co-located with the world it edits.
	Namespace string
	// ServiceAccount is the weak SA the Pod runs as. It reuses felis-restore (bare,
	// no Role/RoleBinding anywhere): a file-editor Pod needs no K8s API access, only
	// filesystem access to the one PVC it mounts, so a second identity with the same
	// empty powers would be a manifest to maintain for no isolation gain.
	ServiceAccount string
	// Image is the felis binary image; the Job runs `felis files` from it.
	Image string
	// WorldsRoot is the in-Pod mount path of the world PVC, and therefore the root
	// every caller-supplied path is resolved against. It defaults to the operator's
	// own dataMountPath ("/data") rather than restore's "/world" so the paths a user
	// types are the paths the MINECRAFT SERVER sees: "server.properties" means the
	// same file in the editor as it does in every wiki page and support thread.
	WorldsRoot string
	// Deadline caps the Pod's wall-clock (activeDeadlineSeconds).
	Deadline time.Duration
	// Timeout caps how long felis-api waits for a result before giving up. It bounds
	// an HTTP handler's block, so it is the tighter of the two: a Pod that is still
	// pulling its image when this expires leaves the caller with a clean 504 while
	// the Job runs on harmlessly to its own Deadline and is then TTL'd away.
	Timeout time.Duration
	// CPULimit / MemLimit cap the container.
	CPULimit string
	MemLimit string
	// RunAsUser / RunAsGroup / FSGroup are the Pod's runtime identity. They default
	// to ROOT (0:0): the world volume belongs to the game uid (naming.GameUID), and
	// Paper saves mode-0600 files a different non-root uid can neither read nor
	// rewrite (level.dat). DAC_OVERRIDE on the container reaches them, CHOWN on a
	// write hands the created file back to the game uid; FSGroup is omitted when
	// zero.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// TTLAfterFinished is how long a finished Job lingers before the Job controller
	// collects it. felis-api holds no jobs:delete, so this is the ONLY cleanup path;
	// it must stay comfortably longer than the moment felis-api needs to read the
	// Pod's log, because the TTL takes the Pod (and its log) with the Job.
	TTLAfterFinished time.Duration

	// AsyncDeadline, AsyncTTL and AsyncCPULimit stand in for Deadline,
	// TTLAfterFinished and CPULimit on an upload or unzip felis-api starts and
	// does not wait on. Such a Job moves a whole archive or a file of gigabytes,
	// so it gets hours; it stays after finishing long enough for the panel to
	// show how it ended; and it gets a whole core, since inflating is CPU-bound.
	AsyncDeadline time.Duration
	AsyncTTL      time.Duration
	AsyncCPULimit string
}

// defaults applied when a Config field is left zero. They are sized for what a
// file operation actually is — open one file, print a few KiB — which is orders of
// magnitude smaller than a restore's tar of an entire world.
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

// withDefaults returns a copy of c with zero fields filled, so a partially
// configured Config (or the zero value, in tests) is always usable.
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

// Editor is the production internal/api.FileEditor. It holds no mutable state.
type Editor struct {
	Runner Runner
	Config Config
}

// Listing is one directory as List returns it.
type Listing struct {
	Entries []Entry
	// Truncated reports that the directory holds more than MaxEntries.
	Truncated bool
	// Free is the bytes free on the server's volume, negative when the Job could
	// not tell.
	Free int64
}

// List returns one directory's entries, resolved under the server's world root.
// An empty path lists the world root itself.
func (e *Editor) List(ctx context.Context, server, path string) (Listing, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpList, Path: path})
	if err != nil {
		return Listing{}, err
	}
	// A genuinely empty directory unmarshals Entries as nil; normalise it so the
	// handler serialises [] rather than null.
	if res.Entries == nil {
		res.Entries = []Entry{}
	}
	return Listing{Entries: res.Entries, Truncated: res.Truncated, Free: res.Avail}, nil
}

// Read returns a file's bytes, resolved under the server's world root, and the
// SHA-256 of the file as it is on disk — the value to hand back as Write's expect.
func (e *Editor) Read(ctx context.Context, server, path string) ([]byte, string, error) {
	res, err := e.run(ctx, server, JobParams{Op: OpRead, Path: path})
	if err != nil {
		return nil, "", err
	}
	// A zero-length file unmarshals Content as nil, which is a legitimate result,
	// not an error — normalise so the caller never has to distinguish nil from empty.
	if res.Content == nil {
		res.Content = []byte{}
	}
	return res.Content, res.SHA256, nil
}

// Write atomically replaces a file's contents, creating it if absent (but never
// creating parent directories — see the write helper in exec.go), and returns the
// new SHA-256. A non-empty expect makes it conditional: ErrConflict if the file no
// longer hashes to it. createOnly refuses a path that exists with ErrExists.
func (e *Editor) Write(ctx context.Context, server, path string, content []byte, expect string, createOnly bool) (string, error) {
	res, err := e.run(ctx, server, JobParams{
		Op: OpWrite, Path: path, Content: content, Expect: expect, CreateOnly: createOnly,
	})
	if err != nil {
		return "", err
	}
	return res.SHA256, nil
}

// Mkdir makes one directory; its parent must exist.
func (e *Editor) Mkdir(ctx context.Context, server, path string) error {
	_, err := e.run(ctx, server, JobParams{Op: OpMkdir, Path: path})
	return err
}

// Delete removes a file, a link, or a directory with everything in it.
func (e *Editor) Delete(ctx context.Context, server, path string) error {
	_, err := e.run(ctx, server, JobParams{Op: OpDelete, Path: path})
	return err
}

// Rename moves path to to. It never replaces an existing destination.
func (e *Editor) Rename(ctx context.Context, server, path, to string) error {
	_, err := e.run(ctx, server, JobParams{Op: OpRename, Path: path, To: to})
	return err
}

// UploadSource is where an upload Job fetches its bytes: a one-time URL on
// felis-api's internal face and the token that opens it (see Stage), plus the size
// and SHA-256 the fetched bytes must match.
type UploadSource struct {
	URL    string
	Token  string
	Size   int64
	SHA256 string
}

// Upload lands the staged bytes at path. The Job refuses bytes that do not match
// src.Size and src.SHA256, so a nil error means exactly those landed. overwrite
// lets it replace an existing file; without it an existing path is ErrExists.
func (e *Editor) Upload(ctx context.Context, server, path string, src UploadSource, overwrite bool) error {
	_, err := e.run(ctx, server, JobParams{
		Op: OpUpload, Path: path, Overwrite: overwrite,
		SourceURL: src.URL, UploadToken: src.Token, UploadSize: src.Size, UploadSHA256: src.SHA256,
	})
	return err
}

// run is the shared body of every operation: mint an op id, fill the rest of the
// params from the Config, run the Job, and translate the Result's code into a
// sentinel error. p carries the op and its own fields.
//
// The size check happens HERE, before a Job is created, as well as inside the Pod.
// That is not redundancy for its own sake: an oversized write would otherwise be
// rejected by the API SERVER (etcd's object limit) as an opaque failure, long after
// felis-api had committed to the request, instead of as a clean 413.
func (e *Editor) run(ctx context.Context, server string, p JobParams) (Result, error) {
	if p.Op == OpWrite && len(p.Content) > MaxWriteBytes {
		return Result{}, fmt.Errorf("%w: content is %d bytes, the limit is %d",
			ErrTooLarge, len(p.Content), MaxWriteBytes)
	}

	cfg := e.Config.withDefaults()
	p, err := cfg.params(server, p)
	if err != nil {
		return Result{}, err
	}

	// Bound the wait here rather than trusting the caller's context: this is an HTTP
	// handler's goroutine and the Pod it waits on may never become ready (an
	// unschedulable node, an unpullable image). The Job's own activeDeadlineSeconds
	// cleans up the cluster side independently.
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

// params fills in what every Job of server takes from the Config, and a fresh op
// id; p carries the op and its own fields.
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

// The states of an OpState.
const (
	OpRunning   = "running"
	OpSucceeded = "succeeded"
	OpFailed    = "failed"
)

// OpState is where one background file operation stands.
type OpState struct {
	ID    string
	Op    string
	Path  string
	State string
	// Started is when the Job was created; Finished when it ended, zero while it
	// runs.
	Started  time.Time
	Finished time.Time
	// Done and Total are the bytes of the latest progress line, zero before the
	// first.
	Done, Total int64
	// Result is what the Job printed once it finished. It is nil while the Job
	// runs, and for a Job that ended without printing one (killed at its
	// deadline, out of memory, its bytes unfetchable), whose Reason says why
	// (ReasonOOMKilled for memory).
	Result *Result
	Reason string
}

// StartUpload starts landing the staged bytes src describes at path and returns
// without waiting, for a file too big to land inside one request (Upload). The
// Job checks what Upload's does; Ops reports how it ends.
func (e *Editor) StartUpload(ctx context.Context, server, path string, src UploadSource, overwrite bool) (OpState, error) {
	return e.start(ctx, server, JobParams{
		Op: OpUpload, Path: path, Overwrite: overwrite,
		SourceURL: src.URL, UploadToken: src.Token, UploadSize: src.Size, UploadSHA256: src.SHA256,
	})
}

// StartUnzip starts extracting the .zip at path into the folder holding it and
// returns without waiting. Without overwrite an archive that would replace a
// file changes nothing and ends with CodeExists and the list (Result.Conflicts).
func (e *Editor) StartUnzip(ctx context.Context, server, path string, overwrite bool) (OpState, error) {
	return e.start(ctx, server, JobParams{Op: OpUnzip, Path: path, Overwrite: overwrite})
}

// Ops reports the server's background operations the cluster still holds: the
// one running, if any, and those finished within AsyncTTL.
func (e *Editor) Ops(ctx context.Context, server string) ([]OpState, error) {
	return e.Runner.Ops(ctx, e.Config.withDefaults().Namespace, server)
}

func (e *Editor) start(ctx context.Context, server string, p JobParams) (OpState, error) {
	cfg := e.Config.withDefaults()
	p, err := cfg.params(server, p)
	if err != nil {
		return OpState{}, err
	}
	p.Async = true
	p.Deadline, p.TTLAfterFinished, p.CPULimit = cfg.AsyncDeadline, cfg.AsyncTTL, cfg.AsyncCPULimit
	if err := e.Runner.Start(ctx, p); err != nil {
		return OpState{}, err
	}
	return OpState{ID: p.OpID, Op: p.Op, Path: p.Path, State: OpRunning, Started: time.Now()}, nil
}

// resultError translates a Result's code into the sentinel the API maps. An
// unrecognised code is deliberately NOT swallowed as success: a Job reporting a
// failure this build does not know about must still fail the request, or a future
// code would silently read as "it worked".
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
	case CodeConflict:
		return fmt.Errorf("%w: %s", ErrConflict, res.Error)
	case CodeNoSpace:
		return fmt.Errorf("%w: %s", ErrNoSpace, res.Error)
	case CodeExists:
		return fmt.Errorf("%w: %s", ErrExists, res.Error)
	default:
		return fmt.Errorf("fileedit: file operation failed (%s): %s", res.Code, res.Error)
	}
}

// newOpID mints the per-invocation tag that names the Job and labels its Pod. 64
// bits of randomness is far more than collision-avoidance needs (a collision only
// matters between two operations alive in the same TTL window), but the id is also
// what selects THIS operation's Pod when reading the result back — so a collision
// would mean reading another operation's output, and the margin is cheap. Hex
// keeps it a valid DNS-1123 name fragment and a valid label value.
func newOpID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fileedit: generate op id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
