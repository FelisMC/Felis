// Package fileedit implements the server file editor (list / read / write a file
// in a server's world volume), the lever an owner reaches for when a server will
// not boot because one line of server.properties or a plugin's YAML is wrong —
// the one repair that otherwise requires a human with cluster access.
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
// the other way, on the Job spec felis-api creates (see ContentEnv).
//
// The price is latency: every operation is a Pod schedule + image pull, so a
// listing takes seconds rather than milliseconds. That is inherent to RWO plus a
// stopped server, not a property of this transport, and it is why the editor is a
// repair tool rather than a file manager.
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
)

// Runner is the cluster-side half of one file operation: render and create the
// Job, wait for its Pod to reach a terminal phase, and return the marked JSON
// payload the Pod printed. It is one method rather than a create/poll/read trio
// because felis-api cannot poll a Job at all (no jobs:get — see FilesJobName), so
// there is no intermediate state a caller could usefully observe; the operation is
// synchronous from the API's point of view whether or not the seam pretends
// otherwise.
//
// It is an interface so the Editor's orchestration and error mapping are tested
// against a fake; the client-go implementation (K8sRunner) is integration-only.
type Runner interface {
	// Run creates the Job for p and returns the raw JSON payload from the
	// ResultPrefix line of its Pod's log.
	Run(ctx context.Context, p JobParams) ([]byte, error)
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
	// RunAsUser / RunAsGroup / FSGroup are the Pod's runtime identity. FSGroup MUST
	// match the operator StatefulSet's runtime group, or a file this Pod writes
	// would be unreadable by the minecraft server that later mounts the same PVC.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// TTLAfterFinished is how long a finished Job lingers before the Job controller
	// collects it. felis-api holds no jobs:delete, so this is the ONLY cleanup path;
	// it must stay comfortably longer than the moment felis-api needs to read the
	// Pod's log, because the TTL takes the Pod (and its log) with the Job.
	TTLAfterFinished time.Duration
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
	defaultRunAsID        = int64(1000)
	defaultTTL            = 2 * time.Minute
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
	if c.RunAsUser == 0 {
		c.RunAsUser = defaultRunAsID
	}
	if c.RunAsGroup == 0 {
		c.RunAsGroup = defaultRunAsID
	}
	if c.FSGroup == 0 {
		c.FSGroup = defaultRunAsID
	}
	if c.TTLAfterFinished <= 0 {
		c.TTLAfterFinished = defaultTTL
	}
	return c
}

// Editor is the production internal/api.FileEditor. It holds no mutable state.
type Editor struct {
	Runner Runner
	Config Config
}

// List returns one directory's entries, resolved under the server's world root.
// An empty path lists the world root itself.
func (e *Editor) List(ctx context.Context, server, path string) ([]Entry, bool, error) {
	res, err := e.run(ctx, server, OpList, path, nil)
	if err != nil {
		return nil, false, err
	}
	// A genuinely empty directory unmarshals Entries as nil; normalise it so the
	// handler serialises [] rather than null.
	if res.Entries == nil {
		res.Entries = []Entry{}
	}
	return res.Entries, res.Truncated, nil
}

// Read returns a file's bytes, resolved under the server's world root.
func (e *Editor) Read(ctx context.Context, server, path string) ([]byte, error) {
	res, err := e.run(ctx, server, OpRead, path, nil)
	if err != nil {
		return nil, err
	}
	// A zero-length file unmarshals Content as nil, which is a legitimate result,
	// not an error — normalise so the caller never has to distinguish nil from empty.
	if res.Content == nil {
		res.Content = []byte{}
	}
	return res.Content, nil
}

// Write replaces a file's contents, creating it if absent (but never creating
// parent directories — see the write helper in exec.go).
func (e *Editor) Write(ctx context.Context, server, path string, content []byte) error {
	_, err := e.run(ctx, server, OpWrite, path, content)
	return err
}

// run is the shared body of all three operations: mint an op id, render the
// params, run the Job, and translate the Result's code into a sentinel error.
//
// The size check happens HERE, before a Job is created, as well as inside the Pod.
// That is not redundancy for its own sake: an oversized write would otherwise be
// rejected by the API SERVER (etcd's object limit) as an opaque failure, long after
// felis-api had committed to the request, instead of as a clean 413.
func (e *Editor) run(ctx context.Context, server, op, path string, content []byte) (Result, error) {
	if op == OpWrite && len(content) > MaxWriteBytes {
		return Result{}, fmt.Errorf("%w: content is %d bytes, the limit is %d",
			ErrTooLarge, len(content), MaxWriteBytes)
	}

	cfg := e.Config.withDefaults()
	opID, err := newOpID()
	if err != nil {
		return Result{}, err
	}

	// Bound the wait here rather than trusting the caller's context: this is an HTTP
	// handler's goroutine and the Pod it waits on may never become ready (an
	// unschedulable node, an unpullable image). The Job's own activeDeadlineSeconds
	// cleans up the cluster side independently.
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	payload, err := e.Runner.Run(ctx, JobParams{
		Server:           server,
		OpID:             opID,
		Op:               op,
		Path:             path,
		Content:          content,
		WorldPVC:         naming.WorldPVCName(server),
		Namespace:        cfg.Namespace,
		ServiceAccount:   cfg.ServiceAccount,
		Image:            cfg.Image,
		WorldsRoot:       cfg.WorldsRoot,
		Deadline:         cfg.Deadline,
		CPULimit:         cfg.CPULimit,
		MemLimit:         cfg.MemLimit,
		RunAsUser:        cfg.RunAsUser,
		RunAsGroup:       cfg.RunAsGroup,
		FSGroup:          cfg.FSGroup,
		TTLAfterFinished: cfg.TTLAfterFinished,
	})
	if err != nil {
		return Result{}, err
	}

	var res Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return Result{}, fmt.Errorf("fileedit: malformed result from the file Job: %w", err)
	}
	return res, resultError(res)
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
