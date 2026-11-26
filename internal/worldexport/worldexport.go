// Package worldexport streams world, backup, and file exports through a one-shot Job to felis-api.
package worldexport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type Request struct {
	Server       string
	Mode         string
	BackupRef    string
	BackupSHA256 string
	Path         string
	Paths        []string
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
	if r.Token == "" {
		return "", fmt.Errorf("worldexport: an export needs a token")
	}
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
	secretName := TokenSecretName(r.ID)
	job, err := ExportJob(JobParams{
		Server: r.Server, ID: r.ID, Mode: r.Mode,
		NodeName: node,
		WorldPVC: claim, BackupPVC: c.BackupPVC, BackupRef: r.BackupRef,
		BackupSHA256: r.BackupSHA256, Path: r.Path, Paths: r.Paths, Dir: r.Dir,
		TargetURL: r.TargetURL, TokenSecretName: secretName,
		Namespace: c.Namespace, ServiceAccount: c.ServiceAccount, Image: c.Image,
		BackupRoot: c.BackupRoot, WorldsRoot: c.WorldsRoot, Deadline: c.Deadline,
		CPULimit: c.CPULimit, MemLimit: c.MemLimit,
		RunAsUser: c.RunAsUser, RunAsGroup: c.RunAsGroup, FSGroup: c.FSGroup,
		TTLAfterFinished: c.TTLAfterFinished,
	})
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: c.Namespace,
			Labels:    job.Labels,
		},
		Data: map[string][]byte{TokenSecretKey: []byte(r.Token)},
	}
	if _, err := e.cs.CoreV1().Secrets(c.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("worldexport: create token secret: %w", err)
	}
	created, err := e.cs.BatchV1().Jobs(c.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		_ = e.cs.CoreV1().Secrets(c.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		return "", fmt.Errorf("worldexport: create export job: %w", err)
	}
	if created.UID != "" {
		patch, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{
				"ownerReferences": []metav1.OwnerReference{{
					APIVersion:         "batch/v1",
					Kind:               "Job",
					Name:               created.Name,
					UID:                created.UID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				}},
			},
		})
		if _, err := e.cs.CoreV1().Secrets(c.Namespace).Patch(ctx, secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			_ = e.Stop(ctx, created.Name)
			return "", fmt.Errorf("worldexport: own token secret: %w", err)
		}
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
	if strings.HasPrefix(job, "export-") {
		if i := strings.LastIndexByte(job, '-'); i > len("export-") {
			if derr := e.cs.CoreV1().Secrets(e.cfg.Namespace).Delete(ctx, TokenSecretName(job[i+1:]), metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("worldexport: delete token secret: %w", derr)
			}
		}
	}
	return nil
}
