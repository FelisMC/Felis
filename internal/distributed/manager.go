// Package distributed extends the existing Job executors with archive transport and stopped migration.
package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/restore"
	"felis.lolicon.best/internal/worldexport"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const LabelTransfer = "felis.lolicon.best/archive-job"

type Backup struct {
	ID      string                  `json:"id"`
	Server  string                  `json:"server"`
	Owner   string                  `json:"owner"`
	Reason  string                  `json:"reason"`
	Protect string                  `json:"protect,omitempty"`
	Receipt archivetransfer.Receipt `json:"receipt"`
}

type pendingBackup struct {
	Ticket                 archivetransfer.Ticket
	Owner, Reason, Protect string
}

type Manager struct {
	Client                       client.Client
	Namespace, Image, Controller string
	Archive                      archivetransfer.Client
	Resolve                      placement.Resolver
	// Record is idempotent by transfer ID and runs on A after durable archive commit.
	Record func(context.Context, Backup) error
}

func (m *Manager) CreateBackupJob(ctx context.Context, p backupjob.JobParams) error {
	world, err := m.Resolve(ctx, p.Server)
	if err != nil {
		return err
	}
	job, t, err := m.uploadJob(p.Server, world.Claim, world.Node, p.JobName)
	if err != nil {
		return err
	}
	reason := "manual"
	if p.Scheduled {
		reason = backupjob.ReasonScheduled
	}
	if p.RestoreRef != "" {
		reason = backupjob.ReasonPreRestore
	}
	meta := pendingBackup{Ticket: t, Owner: p.FormerOwner, Reason: reason, Protect: p.RestoreBackupID}
	raw, _ := json.Marshal(meta)
	job.Annotations = map[string]string{archivetransfer.Annotation: string(raw)}
	job.Labels[maintenance.LabelManagedBy] = "felis-backup"
	job.Labels[archivetransfer.LabelPending] = "true"
	if p.RestoreRef != "" {
		job.Labels[maintenance.LabelThenRestore] = maintenance.ThenRestorePending
		job.Annotations[maintenance.AnnotationRestoreRef] = p.RestoreRef
		job.Annotations[maintenance.AnnotationRestoreBackupID] = p.RestoreBackupID
	}
	job.Spec.TTLSecondsAfterFinished = nil
	err = m.Client.Create(ctx, job)
	if apierrors.IsAlreadyExists(err) {
		return backupjob.ErrAlreadyExists
	}
	return err
}

func (m *Manager) uploadJob(server, claim, node, name string) (*batchv1.Job, archivetransfer.Ticket, error) {
	t, url, token, err := m.Archive.Issue(server, http.MethodPut, "", "", 2*time.Hour)
	if err != nil {
		return nil, t, err
	}
	job, err := worldexport.ExportJob(worldexport.JobParams{Server: server, ID: t.ID[:16], Mode: worldexport.ModeWorld, WorldPVC: claim, TargetURL: url, Token: token, Namespace: m.Namespace, ServiceAccount: "felis-restore", Image: m.Image, WorldsRoot: "/world", Deadline: 2 * time.Hour})
	if err != nil {
		return nil, t, err
	}
	job.Name = name
	job.Labels[LabelTransfer] = "true"
	job.Spec.Template.Labels[LabelTransfer] = "true"
	job.Spec.Template.Spec.Containers[0].Args = append(job.Spec.Template.Spec.Containers[0].Args, "--archive-raw")
	m.pin(&job.Spec.Template.Spec, node)
	return job, t, nil
}

func (m *Manager) pin(p *corev1.PodSpec, node string) {
	if node == "" {
		node = m.Controller
	}
	p.NodeSelector = map[string]string{placement.LabelIdentity: node}
}

func (m *Manager) restoreParams(ctx context.Context, p restore.JobParams) (restore.JobParams, error) {
	rec, err := m.Archive.Inspect(ctx, p.BackupRef)
	if err != nil {
		return p, err
	}
	_, url, token, err := m.Archive.Issue(p.Server, http.MethodGet, p.BackupRef, rec.SHA256, 2*time.Hour)
	if err != nil {
		return p, err
	}
	p.SourceURL, p.Token, p.SHA256, p.MaxBytes = url, token, rec.SHA256, m.Archive.Limit
	if p.MaxBytes <= 0 {
		p.MaxBytes = archivetransfer.DefaultLimit
	}
	p.BackupPVC = ""
	return p, nil
}

func (m *Manager) CreateRestoreJob(ctx context.Context, p restore.JobParams) error {
	world, err := m.Resolve(ctx, p.Server)
	if err != nil {
		return err
	}
	p.WorldPVC = world.Claim
	p, err = m.restoreParams(ctx, p)
	if err != nil {
		return err
	}
	// Preserve the existing deterministic-name conflict and finished-Job retry rules.
	return restore.NewK8sJobs(&pinnedClient{Client: m.Client, node: world.Node, manager: m}).CreateRestoreJob(ctx, p)
}

type pinnedClient struct {
	client.Client
	node    string
	manager *Manager
}

func (c *pinnedClient) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if j, ok := o.(*batchv1.Job); ok {
		c.manager.pin(&j.Spec.Template.Spec, c.node)
		j.Labels[LabelTransfer] = "true"
		j.Spec.Template.Labels[LabelTransfer] = "true"
	}
	return c.Client.Create(ctx, o, opts...)
}

// SettleBackups is retried after A restarts. Jobs remain durable until the row is recorded.
func (m *Manager) SettleBackups(ctx context.Context) error {
	var list batchv1.JobList
	if err := m.Client.List(ctx, &list, client.InNamespace(m.Namespace), client.MatchingLabels{archivetransfer.LabelPending: "true"}); err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		j := &list.Items[i]
		var p pendingBackup
		if err := json.Unmarshal([]byte(j.Annotations[archivetransfer.Annotation]), &p); err != nil {
			errs = append(errs, err)
			continue
		}
		rec, err := m.Archive.Receipt(ctx, p.Ticket.ID)
		if err != nil {
			if !maintenance.JobFinished(j) {
				continue
			}
			// A terminal upload failure holds nothing, but must never start its restore chain.
			if jobSucceeded(j) {
				errs = append(errs, fmt.Errorf("backup %s awaits durable receipt: %w", j.Name, err))
				continue
			}
		} else {
			if rec.Ref != p.Ticket.Ref || rec.SHA256 == "" || rec.Size <= 0 || rec.Size > p.Ticket.Limit {
				errs = append(errs, fmt.Errorf("invalid receipt for %s", j.Name))
				continue
			}
			if m.Record == nil {
				errs = append(errs, errors.New("backup recorder unavailable"))
				continue
			}
			if err := m.Record(ctx, Backup{ID: p.Ticket.ID, Server: p.Ticket.Server, Owner: p.Owner, Reason: p.Reason, Protect: p.Protect, Receipt: rec}); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		before := j.DeepCopy()
		delete(j.Labels, archivetransfer.LabelPending)
		ttl := int32(600)
		j.Spec.TTLSecondsAfterFinished = &ttl
		if err := m.Client.Patch(ctx, j, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func jobSucceeded(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// RemoteArchiver lets the existing reaper make every decision on A and snapshot only one remote PVC.
// Local verification, retention and offsite continue to use the same tarLocal paths.
type RemoteArchiver struct {
	*backup.TarLocal
	Manager *Manager
}

// A failed migration may wait for operator intervention longer than normal
// backup retention. Keep its safety archive until the persistent lock releases.
func (a *RemoteArchiver) Delete(ctx context.Context, ref backup.ArchiveRef) error {
	var servers v1alpha1.MinecraftServerList
	if err := a.Manager.Client.List(ctx, &servers, client.InNamespace(a.Manager.Namespace)); err != nil {
		return err
	}
	for i := range servers.Items {
		op, err := readOperation(&servers.Items[i])
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && op.State != "succeeded" && op.Backup.Ref == string(ref) {
			return fmt.Errorf("%w: migration retains its safety archive", ErrBusy)
		}
	}
	return a.TarLocal.Delete(ctx, ref)
}

func (a *RemoteArchiver) Archive(ctx context.Context, server, pvc string) (backup.Archived, error) {
	m := a.Manager
	world, err := m.Resolve(ctx, server)
	if err != nil {
		return backup.Archived{}, err
	}
	if world.Claim != pvc {
		return backup.Archived{}, errors.New("active PVC changed")
	}
	id := archivetransfer.ID()
	name := "reap-" + id[:16]
	j, t, err := m.uploadJob(server, pvc, world.Node, name)
	if err != nil {
		return backup.Archived{}, err
	}
	if err = m.Client.Create(ctx, j); err != nil {
		return backup.Archived{}, err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		rec, err := m.Archive.Receipt(ctx, t.ID)
		if err == nil {
			return backup.Archived{Ref: backup.ArchiveRef(rec.Ref), Size: rec.Size, SHA256: rec.SHA256}, nil
		}
		var job batchv1.Job
		if err = m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, &job); err != nil {
			return backup.Archived{}, err
		}
		if maintenance.JobFinished(&job) && !jobSucceeded(&job) {
			return backup.Archived{}, errors.New("remote archive Job failed")
		}
		select {
		case <-ctx.Done():
			return backup.Archived{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
