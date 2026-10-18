// Package backup archives and restores worlds through opaque backend-specific references.
package backup

import (
	"context"
	"errors"
	"time"
)

type ArchiveRef string

type WorldArchiver interface {
	// Archive returns only after durable completion; interruptions must not leave a finished-looking archive.
	Archive(ctx context.Context, server, pvc string) (Archived, error)
	Restore(ctx context.Context, ref ArchiveRef, targetPVC string) error
	Delete(ctx context.Context, ref ArchiveRef) error
}

type Archived struct {
	Ref  ArchiveRef
	Size int64
	// SHA256 is empty when the backend does not record a digest.
	SHA256 string
	// Skipped names unsupported entries relative to the world root.
	Skipped []string
}

// ErrCorrupt alone condemns archive contents; unreadable mounts and cancelled operations do not establish corruption.
var ErrCorrupt = errors.New("backup: archive corrupt")

// Verifier checks the full archive and optional expected digest; an empty want establishes a digest for older archives.
type Verifier interface {
	Verify(ctx context.Context, ref ArchiveRef, want string) (sha256 string, err error)
}

type Sweeper interface {
	// Sweep removes partials before partialBefore and finished unreferenced archives only before orphanBefore.
	Sweep(ctx context.Context, live func(ArchiveRef) bool, partialBefore, orphanBefore time.Time) (Swept, error)
}

type Swept struct {
	Removed []string
	// Keep young orphan archives because database restore or a failed record insert may temporarily leave the only world copy unreferenced.
	Orphans     []string
	OrphanBytes int64
}

type PVCResolver func(pvc string) (string, error)

func StaticResolver(paths map[string]string) PVCResolver {
	return func(pvc string) (string, error) {
		if p, ok := paths[pvc]; ok {
			return p, nil
		}
		return "", &UnknownPVCError{PVC: pvc}
	}
}

type UnknownPVCError struct{ PVC string }

func (e *UnknownPVCError) Error() string { return "backup: unknown pvc " + e.PVC }
