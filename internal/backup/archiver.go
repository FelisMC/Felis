// Package backup implements the WorldArchiver abstraction (spec §19). The
// reaper and the restore endpoint speak only to the interface and never learn
// whether the backend is a tar file, a VolumeSnapshot, or a Longhorn backup —
// ArchiveRef is deliberately opaque.
package backup

import (
	"context"
	"errors"
	"time"
)

// ArchiveRef is an opaque handle to a stored world archive. Depending on the
// backend it may be a tar path, a VolumeSnapshot name, or a Longhorn backup URL.
type ArchiveRef string

// WorldArchiver archives, restores, and deletes a server's world. The signature
// is intentionally "archive a world" rather than "write bytes": snapshot
// backends (VolumeSnapshot/Longhorn) cannot produce an io.Reader — they create
// K8s objects referencing the source PVC (spec §19).
type WorldArchiver interface {
	// Archive captures the world living on pvc for server. It returns only once
	// the archive is complete and durable: a failed or interrupted Archive leaves
	// nothing that could be mistaken for a finished archive.
	Archive(ctx context.Context, server, pvc string) (Archived, error)
	// Restore writes a previously archived world into targetPVC.
	Restore(ctx context.Context, ref ArchiveRef, targetPVC string) error
	// Delete removes the archive identified by ref.
	Delete(ctx context.Context, ref ArchiveRef) error
}

// Archived is what one Archive call stored.
type Archived struct {
	Ref  ArchiveRef
	Size int64 // stored bytes
	// SHA256 is the hex digest of the stored archive, "" when the backend keeps
	// none. world_backups.sha256 records it, so a later Verify tells an archive
	// that rotted on disk from a good one.
	SHA256 string
	// Skipped lists the world entries the archive leaves out (symbolic links,
	// devices, sockets, named pipes), relative to the world root.
	Skipped []string
}

// ErrCorrupt marks an archive that cannot be read back in full, or whose bytes
// no longer match the checksum recorded for it. It is the only Verify error that
// condemns the archive: any other (a mount that is not there, a cancelled
// context) says nothing about it.
var ErrCorrupt = errors.New("backup: archive corrupt")

// Verifier is implemented by backends that can read a stored archive back end to
// end. Verify returns the archive's SHA256 as read; want, when not "", is the
// digest it must match (an archive recorded before checksums were kept has
// none, and its read-back establishes one).
type Verifier interface {
	Verify(ctx context.Context, ref ArchiveRef, want string) (sha256 string, err error)
}

// Sweeper is implemented by backends that can find what interrupted archives
// leave behind.
type Sweeper interface {
	// Sweep removes every unfinished archive last written before partialBefore.
	// A finished archive that live does not claim (its record was never inserted)
	// is removed once it was last written before orphanBefore, and reported and
	// kept until then.
	Sweep(ctx context.Context, live func(ArchiveRef) bool, partialBefore, orphanBefore time.Time) (Swept, error)
}

// Swept is what one Sweep found.
type Swept struct {
	Removed []string // paths removed
	// Orphans are finished archives no record claims that are still young enough
	// to keep. A backup whose record insert failed looks like this, and so do the
	// archives of a store brought back before the database that records them:
	// removing them early could throw away the only copy of a world.
	Orphans     []string
	OrphanBytes int64
}

// PVCResolver maps a PVC name to the local filesystem path where it is mounted.
// In production the reaper Job mounts the source/backup PVCs and supplies a
// resolver over those mount points; tests supply temp dirs.
type PVCResolver func(pvc string) (string, error)

// StaticResolver resolves PVC names from a fixed map, erroring on unknown names.
func StaticResolver(paths map[string]string) PVCResolver {
	return func(pvc string) (string, error) {
		if p, ok := paths[pvc]; ok {
			return p, nil
		}
		return "", &UnknownPVCError{PVC: pvc}
	}
}

// UnknownPVCError is returned when a resolver cannot map a PVC name.
type UnknownPVCError struct{ PVC string }

func (e *UnknownPVCError) Error() string { return "backup: unknown pvc " + e.PVC }
