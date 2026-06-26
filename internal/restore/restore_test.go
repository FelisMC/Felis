package restore_test

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/restore"
)

// fakeJobs is an in-memory Jobs that records the params it was handed and
// returns a programmable error, so the orchestration is tested without a
// cluster.
type fakeJobs struct {
	calls []restore.JobParams
	err   error
}

func (f *fakeJobs) CreateRestoreJob(_ context.Context, p restore.JobParams) error {
	f.calls = append(f.calls, p)
	return f.err
}

func TestRestoreEnqueuesJobWithDerivedParams(t *testing.T) {
	jobs := &fakeJobs{}
	r := &restore.Restorer{
		Jobs: jobs,
		Config: restore.Config{
			Image:     "registry.internal/felis:test",
			BackupPVC: "felis-backups",
		},
	}

	if err := r.Restore(context.Background(), "survival", "/backups/survival/2026.tar.gz"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(jobs.calls) != 1 {
		t.Fatalf("CreateRestoreJob called %d times, want 1", len(jobs.calls))
	}
	got := jobs.calls[0]
	if got.Server != "survival" {
		t.Errorf("Server = %q, want survival", got.Server)
	}
	// The world PVC must come from the shared naming convention, not be invented
	// here: it is the same name the operator created and the reaper deletes.
	if got.WorldPVC != "world-survival-0" {
		t.Errorf("WorldPVC = %q, want world-survival-0", got.WorldPVC)
	}
	if got.BackupRef != "/backups/survival/2026.tar.gz" {
		t.Errorf("BackupRef = %q, want the passed ref", got.BackupRef)
	}
	if got.BackupPVC != "felis-backups" {
		t.Errorf("BackupPVC = %q, want felis-backups", got.BackupPVC)
	}
	if got.Image != "registry.internal/felis:test" {
		t.Errorf("Image = %q, want the configured image", got.Image)
	}
	// withDefaults must have filled the unset fields.
	if got.Namespace == "" || got.ServiceAccount == "" || got.WorldsRoot == "" || got.BackupRoot == "" {
		t.Errorf("defaults not applied: %+v", got)
	}
}

func TestRestoreIsIdempotentOnAlreadyExists(t *testing.T) {
	jobs := &fakeJobs{err: restore.ErrAlreadyExists}
	r := &restore.Restorer{Jobs: jobs, Config: restore.Config{Image: "img", BackupPVC: "pvc"}}

	// A restore already in flight is success, not an error: the handler must be
	// able to answer 202 for a coalesced duplicate request.
	if err := r.Restore(context.Background(), "survival", "ref"); err != nil {
		t.Fatalf("Restore on AlreadyExists = %v, want nil (idempotent)", err)
	}
}

func TestRestorePropagatesGenericError(t *testing.T) {
	sentinel := errors.New("apiserver exploded")
	jobs := &fakeJobs{err: sentinel}
	r := &restore.Restorer{Jobs: jobs, Config: restore.Config{Image: "img", BackupPVC: "pvc"}}

	err := r.Restore(context.Background(), "survival", "ref")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Restore error = %v, want the underlying error propagated", err)
	}
}
