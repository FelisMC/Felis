package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/nodecontrol"
)

// Failover of a lost worker, or with controller high availability a lost controller
// (internal/distributed failover.go), from the Owner panel or on its own once automatic
// failover is on, and the background work around it:
//
//   - the plan: the servers whose worlds are on the lost node, each with its newest intact
//     backup, which is what a failover restores and so how much it loses;
//   - the failover itself: one Operation per server towards the chosen node, and a fence
//     task for node-control, which alone may cordon and taint nodes;
//   - FailoverTick, every half minute: fence tasks for the failovers waiting on one, the
//     automatic failover, and the replicas: a live copy of each world running on a node
//     that can be failed over every ReplicaMinutes, so the newest backup a failover
//     restores is recent.
//
// The automatic failover acts only while a controller is up and exactly one node is lost
// (and not fenced yet), for at least AfterMinutes: several nodes vanishing at once looks
// more like the controller's own network failing than like dead machines.

const failoverPolicyKey = "failover_policy"

// FailoverPolicy is the Owner's failover setting.
type FailoverPolicy struct {
	// Automatic fails a lost worker over without waiting for the Owner, once it has been
	// gone AfterMinutes.
	Automatic    bool `json:"automatic"`
	AfterMinutes int  `json:"afterMinutes"`
	// ReplicaMinutes is how often a world running on a worker is copied live; 0 turns the
	// replicas off.
	ReplicaMinutes int `json:"replicaMinutes"`
}

// defaultFailoverPolicy holds until the Owner saves one: failover by hand, hourly replicas.
var defaultFailoverPolicy = FailoverPolicy{AfterMinutes: 15, ReplicaMinutes: 60}

func (p FailoverPolicy) valid() bool {
	return p.AfterMinutes >= 10 && p.AfterMinutes <= 1440 &&
		(p.ReplicaMinutes == 0 || (p.ReplicaMinutes >= 15 && p.ReplicaMinutes <= 1440))
}

type failoverPolicyView struct {
	FailoverPolicy
	Revision string `json:"revision"`
	Managed  bool   `json:"managed"`
}

func (a *API) readFailoverPolicy(ctx context.Context) (failoverPolicyView, []byte, error) {
	view := failoverPolicyView{FailoverPolicy: defaultFailoverPolicy}
	raw, err := a.Repo.GetSetting(ctx, failoverPolicyKey)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		return view, nil, err
	default:
		if err := json.Unmarshal(raw, &view.FailoverPolicy); err != nil {
			return view, nil, err
		}
		if !view.valid() {
			return view, nil, errors.New("invalid persisted failover policy")
		}
		view.Managed = true
	}
	canonical, _ := json.Marshal(view.FailoverPolicy)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) handleGetFailoverPolicy(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readFailoverPolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetFailoverPolicy(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		FailoverPolicy
		Revision string `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !body.valid() {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"automatic failover waits 10–1440 minutes, and replicas run every 15–1440 minutes or not at all"))
		return
	}
	current, expected, err := a.readFailoverPolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "the failover setting changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.FailoverPolicy)
	if err := a.Repo.CompareAndSetSetting(r.Context(), failoverPolicyKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "platform.failover_policy", "platform")
	view, _, err := a.readFailoverPolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// FailoverBackup is the backup a failover would restore.
type FailoverBackup struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	SizeBytes int64     `json:"sizeBytes"`
}

// FailoverServer is one server of a failover plan.
type FailoverServer struct {
	Name         string                 `json:"name"`
	DesiredState string                 `json:"desiredState"`
	Backup       *FailoverBackup        `json:"backup"`
	Operation    *distributed.Operation `json:"operation"`
}

// FailoverPlan is what failing a node over would do.
type FailoverPlan struct {
	Node      string           `json:"node"`
	Lost      bool             `json:"lost"`
	Fenced    bool             `json:"fenced"`
	LostSince *time.Time       `json:"lostSince"`
	Servers   []FailoverServer `json:"servers"`
	Targets   []string         `json:"targets"`
}

// validNode checks a node name from the path the way node-control does.
func validNode(name string) bool {
	return nodecontrol.Request{Action: "fence", Name: name, ConfirmMaintenance: true}.Validate() == nil
}

func (a *API) handleFailoverPlan(w http.ResponseWriter, r *http.Request) {
	if !a.distributedReady(w, r) {
		return
	}
	node := r.PathValue("node")
	if !validNode(node) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_node", "invalid node name"))
		return
	}
	nodes, err := a.Distribution.Nodes(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	plan := FailoverPlan{Node: node, Servers: []FailoverServer{}, Targets: []string{}}
	found := false
	for _, n := range nodes {
		switch {
		case n.Name == node:
			found = true
			plan.Lost, plan.Fenced, plan.LostSince = !n.Ready && n.Role == "worker", n.Fenced, n.LostSince
		case n.Ready && (n.Role == "controller" || (n.Role == "worker" && n.Approved)):
			plan.Targets = append(plan.Targets, n.Name)
		}
	}
	if !found {
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no such node"))
		return
	}
	placed, err := a.Distribution.ServersOn(r.Context(), node)
	if err != nil {
		writeError(w, r, err)
		return
	}
	for _, p := range placed {
		s := FailoverServer{Name: p.Name, DesiredState: p.DesiredState, Operation: p.Operation}
		switch b, err := a.Repo.LatestBackup(r.Context(), p.Name); {
		case err == nil:
			s.Backup = &FailoverBackup{ID: b.ID, CreatedAt: b.CreatedAt, SizeBytes: b.SizeBytes}
		case !errors.Is(err, ErrNotFound):
			writeError(w, r, err)
			return
		}
		plan.Servers = append(plan.Servers, s)
	}
	writeJSON(w, http.StatusOK, plan)
}

// FailoverSkip is a server a failover left where it was, and why.
type FailoverSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"` // no_backup, unknown_server or failed
	Error  string `json:"error,omitempty"`
}

// FailoverResult is what a failover started.
type FailoverResult struct {
	Operations []distributed.Operation `json:"operations"`
	Skipped    []FailoverSkip          `json:"skipped"`
	// FenceTask is the node-control task fencing the node, when this request started one;
	// FenceError says why none could be started (it is retried in the background).
	FenceTask  *nodecontrol.Task `json:"fenceTask,omitempty"`
	FenceError string            `json:"fenceError,omitempty"`
}

func (a *API) handleFailover(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.distributedReady(w, r) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	node := r.PathValue("node")
	if !validNode(node) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_node", "invalid node name"))
		return
	}
	var body struct {
		TargetNode string `json:"targetNode"`
		Confirm    string `json:"confirm"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Confirm != node {
		writeError(w, r, newError(http.StatusBadRequest, "confirm_mismatch", "type the node's name to confirm the failover"))
		return
	}
	if err := a.Distribution.ValidateNode(r.Context(), body.TargetNode); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_node", "%v", err))
		return
	}
	p := principalFromContext(r.Context())
	actor := auditActor(p)
	res, err := a.failOver(r.Context(), node, body.TargetNode, actor)
	if err != nil {
		a.writeFailoverError(w, r, err)
		return
	}
	payload := auditPayload(map[string]any{"target": body.TargetNode, "operations": len(res.Operations), "skipped": res.Skipped})
	e := AuditEntry{Actor: actor, Action: "platform.node.failover", ServerName: node, Payload: payload}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	a.auditEntry(r, e)
	writeJSON(w, http.StatusAccepted, res)
}

func (a *API) writeFailoverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, distributed.ErrNodeNotLost) {
		writeError(w, r, newError(http.StatusConflict, "node_not_lost", "only an offline worker can be failed over"))
		return
	}
	a.writeMigrationError(w, r, err)
}

// failOver starts the failover of every server on node towards target and asks
// node-control to fence node. A server already failing over keeps its own failover; one
// with no intact backup, or that cannot start, is reported and left where it is.
func (a *API) failOver(ctx context.Context, node, target, actor string) (FailoverResult, error) {
	res := FailoverResult{Operations: []distributed.Operation{}, Skipped: []FailoverSkip{}}
	if _, err := a.Distribution.LostNode(ctx, node); err != nil {
		return res, err
	}
	placed, err := a.Distribution.ServersOn(ctx, node)
	if err != nil {
		return res, err
	}
	for _, p := range placed {
		if op := p.Operation; op != nil && op.Kind == distributed.KindFailover && op.State != "failed" {
			res.Operations = append(res.Operations, *op)
			continue
		}
		rec, err := a.Repo.ServerByName(ctx, p.Name)
		if errors.Is(err, ErrNotFound) {
			res.Skipped = append(res.Skipped, FailoverSkip{Name: p.Name, Reason: "unknown_server"})
			continue
		} else if err != nil {
			return res, err
		}
		b, err := a.Repo.LatestBackup(ctx, p.Name)
		if errors.Is(err, ErrNotFound) {
			res.Skipped = append(res.Skipped, FailoverSkip{Name: p.Name, Reason: "no_backup"})
			continue
		} else if err != nil {
			return res, err
		}
		op, err := a.Distribution.BeginFailover(ctx, p.Name, target, rec.OwnerID, archivetransfer.Receipt{Ref: b.BackupRef, Size: b.SizeBytes, SHA256: b.SHA256})
		if err != nil {
			res.Skipped = append(res.Skipped, FailoverSkip{Name: p.Name, Reason: "failed", Error: err.Error()})
			continue
		}
		log.Printf("api: failover of %s from %s to %s started by %s (backup %s)", p.Name, node, target, actor, b.ID)
		res.Operations = append(res.Operations, op)
	}
	if len(res.Operations) > 0 {
		task, err := a.ensureFence(ctx, node, actor)
		res.FenceTask = task
		if err != nil {
			res.FenceError = err.Error()
		}
	}
	return res, nil
}

// fenceRetry is how long after starting a fence task another may be started for the same
// node: long enough for one to finish, short enough that a failover is not held up for
// long by a fence that did not take.
const fenceRetry = 2 * time.Minute

// errNoNodeControl is returned when the host service that fences nodes is not wired.
var errNoNodeControl = errors.New("the host node service is not configured, so the node cannot be fenced; the failover waits")

// ensureFence asks node-control to fence node, unless a fence of it is running or one was
// started within fenceRetry. It returns the task it started, if any.
func (a *API) ensureFence(ctx context.Context, node, actor string) (*nodecontrol.Task, error) {
	if a.NodeControl == nil {
		return nil, errNoNodeControl
	}
	tasks, err := a.NodeControl.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.State == "running" && t.Request.Action == "fence" && t.Request.Name == node {
			return nil, nil
		}
	}
	if _, ok := a.fenceLimiter().reserve(node, fenceRetry); !ok {
		return nil, nil
	}
	task, err := a.NodeControl.Start(ctx, nodecontrol.Request{Action: "fence", Name: node, ConfirmMaintenance: true}, actor)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// failoverState is FailoverTick's memory: the per-node fence throttle and when each world
// last had a replica started.
type failoverState struct {
	once     sync.Once
	fences   *cooldownLimiter
	replicas *cooldownLimiter
}

func (a *API) fenceLimiter() *cooldownLimiter {
	a.failover.once.Do(a.initFailoverState)
	return a.failover.fences
}

func (a *API) replicaLimiter() *cooldownLimiter {
	a.failover.once.Do(a.initFailoverState)
	return a.failover.replicas
}

func (a *API) initFailoverState() {
	a.failover.fences = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	a.failover.replicas = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
}

// Audit identity of the background failover work.
const (
	failoverActor = "failover"
	replicaAction = "backup.replica"
)

// FailoverTick runs the background half of failover once; cmd/felis calls it every half
// minute in distributed mode. Each part runs whatever the others did, and their errors
// are joined.
func (a *API) FailoverTick(ctx context.Context) error {
	if a.Distribution == nil {
		return nil
	}
	var errs []error
	pending, err := a.Distribution.FencePending(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list the fences failovers wait on: %w", err))
	}
	for _, node := range pending {
		if _, err := a.ensureFence(ctx, node, failoverActor); err != nil {
			errs = append(errs, fmt.Errorf("fence %s: %w", node, err))
		}
	}
	policy, _, err := a.readFailoverPolicy(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if policy.Automatic {
		if err := a.autoFailover(ctx, policy.FailoverPolicy); err != nil {
			errs = append(errs, err)
		}
	}
	if policy.ReplicaMinutes > 0 {
		if err := a.startReplica(ctx, time.Duration(policy.ReplicaMinutes)*time.Minute); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// autoFailover fails the one lost worker over once it has been gone policy.AfterMinutes,
// towards the least loaded online node. A server whose failover failed is left for the
// Owner: retrying it on its own could loop.
func (a *API) autoFailover(ctx context.Context, policy FailoverPolicy) error {
	lost, controllerUp, err := a.Distribution.LostNodes(ctx)
	if err != nil || !controllerUp || len(lost) != 1 {
		return err
	}
	for node, since := range lost {
		if since.IsZero() || a.now().Sub(since) < time.Duration(policy.AfterMinutes)*time.Minute {
			return nil
		}
		placed, err := a.Distribution.ServersOn(ctx, node)
		if err != nil {
			return err
		}
		due := false
		for _, p := range placed {
			if p.Operation == nil || p.Operation.Kind != distributed.KindFailover {
				due = true
			}
		}
		if !due {
			return nil
		}
		target, err := a.Distribution.FailoverTarget(ctx)
		if err != nil {
			return fmt.Errorf("automatic failover of %s: %w", node, err)
		}
		res, err := a.failOver(ctx, node, target, failoverActor)
		if err != nil {
			return fmt.Errorf("automatic failover of %s: %w", node, err)
		}
		log.Printf("api: automatic failover of %s to %s: %d started, %d skipped", node, target, len(res.Operations), len(res.Skipped))
		a.writeAudit(ctx, AuditEntry{Actor: failoverActor, Source: failoverActor, Action: "platform.node.failover", ServerName: node,
			Payload: auditPayload(map[string]any{"target": target, "automatic": true, "operations": len(res.Operations), "skipped": res.Skipped})})
	}
	return nil
}

// startReplica starts at most one replica per tick: the first world running on a worker
// whose newest intact backup is older than every, and which has not had a replica started
// within every. A world whose game did not answer its console waits as long; one that is
// starting, stopping or busy is simply tried again on the next tick. It waits while any
// backup or restore Job runs, so replicas queue behind each other and behind the owners'
// own operations.
func (a *API) startReplica(ctx context.Context, every time.Duration) error {
	if jobs, ok := a.JobStatus.(WorldJobCounter); ok {
		if n, err := jobs.RunningWorldJobs(ctx); err != nil || n > 0 {
			return err
		}
	}
	names, err := a.Distribution.ReplicaCandidates(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !a.replicaLimiter().allowed(name, every) {
			continue
		}
		rec, err := a.Repo.ServerByName(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if b, err := a.Repo.LatestBackup(ctx, name); err == nil && a.now().Sub(b.CreatedAt) < every {
			continue
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		var busy *MaintenanceBusyError
		switch err := a.startLiveBackup(ctx, name, rec.OwnerID, backupjob.ReasonReplica); {
		case err == nil:
		case errors.Is(err, ErrNotRunning), errors.As(err, &busy), errors.Is(err, ErrMaintenanceInProgress), errors.Is(err, errLiveBackupUnavailable):
			continue
		case errors.Is(err, errLiveBackupConsole):
			a.replicaLimiter().record(name)
			continue
		default:
			a.replicaLimiter().record(name)
			return fmt.Errorf("start the replica of %s: %w", name, err)
		}
		a.replicaLimiter().record(name)
		log.Printf("api: started the replica of %s", name)
		a.writeAudit(ctx, AuditEntry{Actor: failoverActor, Source: failoverActor, Action: replicaAction, ServerName: name, Payload: []byte(`{"live":true}`)})
		return nil
	}
	return nil
}
