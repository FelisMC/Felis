// Package worldexport is the executor behind the world export routes (POST
// /servers/{name}/world/export and POST /servers/{name}/backups/{id}/export)
// and the file manager's download (POST /servers/{name}/files/download): a
// one-shot Job in the minecraft namespace that reads a stopped server's world,
// one of its stored archives, or one file or folder of the world, and PUTs it
// to felis-api's internal face, which streams it on to the owner's browser as
// it arrives (internal/api/exports.go). Nothing is staged on the way: felis-api
// never mounts a world or the backup store, and what is sent never lands on a
// disk it owns.
//
// Trust model as in internal/restore: the Pod runs under the weak felis-restore
// SA with no API token, mounts one volume read-only, and holds no database URL
// or Secret. The only credential it gets is the one-time token of this one
// upload. felis-api makes every authorization decision before the Job exists.
package worldexport

import (
	"context"
	"fmt"
	"time"

	"felis.lolicon.best/internal/naming"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Request is one export as felis-api admitted it.
type Request struct {
	Server string
	Mode   string // ModeWorld, ModeBackup or ModeFiles
	// BackupRef is the archive a ModeBackup export reads, and BackupSHA256 what
	// it must hash to.
	BackupRef    string
	BackupSHA256 string
	// Path and Dir are the file or folder a ModeFiles export sends.
	Path string
	Dir  bool
	// ID names the export (JobName) and TargetURL/Token are where and how the
	// Pod hands the archive over.
	ID        string
	TargetURL string
	Token     string
}

// Config parameterises the executor. Image and BackupPVC have no safe default:
// cmd/felis leaves the API's Exporter nil when either is missing, and the
// routes answer 503.
type Config struct {
	Namespace      string
	ServiceAccount string
	Image          string
	BackupPVC      string
	// BackupRoot MUST be the path the archives were written under
	// (cfg.Archive.LocalPath): the stored refs are absolute paths.
	BackupRoot string
	WorldsRoot string
	// Deadline caps the Pod's wall-clock. The archive moves at the browser's
	// pace, so it is longer than a backup's, and it is also the longest a world
	// export can keep the server from starting.
	Deadline         time.Duration
	CPULimit         string
	MemLimit         string
	RunAsUser        int64
	RunAsGroup       int64
	FSGroup          int64
	TTLAfterFinished time.Duration
}

const (
	defaultNamespace      = "minecraft"
	defaultServiceAccount = "felis-restore"
	defaultBackupRoot     = "/backups"
	defaultWorldsRoot     = "/world"
	defaultDeadline       = 2 * time.Hour
	defaultCPULimit       = "1"
	defaultMemLimit       = "256Mi"
	defaultTTL            = 10 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = defaultServiceAccount
	}
	if c.BackupRoot == "" {
		c.BackupRoot = defaultBackupRoot
	}
	if c.WorldsRoot == "" {
		c.WorldsRoot = defaultWorldsRoot
	}
	if c.Deadline <= 0 {
		c.Deadline = defaultDeadline
	}
	return c
}

// Exporter is the production internal/api.Exporter. It creates the Job with
// jobs:create, which felis-api already holds in the minecraft namespace.
type Exporter struct {
	cs  kubernetes.Interface
	cfg Config
}

// New builds an Exporter over the typed clientset.
func New(cs kubernetes.Interface, cfg Config) *Exporter {
	return &Exporter{cs: cs, cfg: cfg.withDefaults()}
}

// Start creates the export Job for r and returns its name.
func (e *Exporter) Start(ctx context.Context, r Request) (string, error) {
	c := e.cfg
	job, err := ExportJob(JobParams{
		Server: r.Server, ID: r.ID, Mode: r.Mode,
		WorldPVC: naming.WorldPVCName(r.Server), BackupPVC: c.BackupPVC, BackupRef: r.BackupRef,
		BackupSHA256: r.BackupSHA256, Path: r.Path, Dir: r.Dir,
		TargetURL: r.TargetURL, Token: r.Token,
		Namespace: c.Namespace, ServiceAccount: c.ServiceAccount, Image: c.Image,
		BackupRoot: c.BackupRoot, WorldsRoot: c.WorldsRoot, Deadline: c.Deadline,
		CPULimit: c.CPULimit, MemLimit: c.MemLimit,
		RunAsUser: c.RunAsUser, RunAsGroup: c.RunAsGroup, FSGroup: c.FSGroup,
		TTLAfterFinished: c.TTLAfterFinished,
	})
	if err != nil {
		return "", err
	}
	if _, err := e.cs.BatchV1().Jobs(c.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("worldexport: create export job: %w", err)
	}
	return job.Name, nil
}

// Stop deletes an export Job felis-api has given up on, so a Pod that never got
// going stops holding the world. Its Pods go with it; one already gone is not
// an error.
func (e *Exporter) Stop(ctx context.Context, job string) error {
	bg := metav1.DeletePropagationBackground
	err := e.cs.BatchV1().Jobs(e.cfg.Namespace).Delete(ctx, job, metav1.DeleteOptions{PropagationPolicy: &bg})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("worldexport: delete export job: %w", err)
	}
	return nil
}
