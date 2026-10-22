// Package worldexport streams world, backup, and file exports through a one-shot Job to felis-api.
package worldexport

import (
	"context"
	"fmt"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Request struct {
	Server       string
	Mode         string
	BackupRef    string
	BackupSHA256 string
	Path         string
	Dir          bool
	ID           string
	TargetURL    string
	Token        string
}

// Config requires Image and BackupPVC; missing values leave export routes unavailable.
type Config struct {
	ResolveWorld   placement.Resolver
	Namespace      string
	ServiceAccount string
	Image          string
	BackupPVC      string
	// BackupRoot must match the archive write path because stored references are absolute.
	BackupRoot string
	WorldsRoot string
	// Deadline also bounds how long a world export prevents the server from starting.
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

type Exporter struct {
	cs  kubernetes.Interface
	cfg Config
}

func New(cs kubernetes.Interface, cfg Config) *Exporter {
	return &Exporter{cs: cs, cfg: cfg.withDefaults()}
}

func (e *Exporter) Start(ctx context.Context, r Request) (string, error) {
	c := e.cfg
	if r.Mode == ModeFiles {
		c.RunAsUser, c.RunAsGroup, c.FSGroup = 1000, 1000, 0
	}
	claim := naming.WorldPVCName(r.Server)
	node := ""
	if c.ResolveWorld != nil && r.Mode != ModeBackup {
		world, err := c.ResolveWorld(ctx, r.Server)
		if err != nil {
			return "", err
		}
		claim, node = world.Claim, world.Node
	}
	job, err := ExportJob(JobParams{
		Server: r.Server, ID: r.ID, Mode: r.Mode,
		NodeName: node,
		WorldPVC: claim, BackupPVC: c.BackupPVC, BackupRef: r.BackupRef,
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

// Stop deletes the Job and its Pods; an already absent Job succeeds.
func (e *Exporter) Stop(ctx context.Context, job string) error {
	bg := metav1.DeletePropagationBackground
	err := e.cs.BatchV1().Jobs(e.cfg.Namespace).Delete(ctx, job, metav1.DeleteOptions{PropagationPolicy: &bg})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("worldexport: delete export job: %w", err)
	}
	return nil
}
