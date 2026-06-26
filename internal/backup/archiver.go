// Package backup implements the WorldArchiver abstraction (spec §19). The
// reaper and the restore endpoint speak only to the interface and never learn
// whether the backend is a tar file, a VolumeSnapshot, or a Longhorn backup —
// ArchiveRef is deliberately opaque.
package backup

import "context"

// ArchiveRef is an opaque handle to a stored world archive. Depending on the
// backend it may be a tar path, a VolumeSnapshot name, or a Longhorn backup URL.
type ArchiveRef string

// WorldArchiver archives, restores, and deletes a server's world. The signature
// is intentionally "archive a world" rather than "write bytes": snapshot
// backends (VolumeSnapshot/Longhorn) cannot produce an io.Reader — they create
// K8s objects referencing the source PVC (spec §19).
type WorldArchiver interface {
	// Archive captures the world living on pvc for server and returns an opaque
	// ref plus the stored size in bytes.
	Archive(ctx context.Context, server, pvc string) (ref ArchiveRef, size int64, err error)
	// Restore writes a previously archived world into targetPVC.
	Restore(ctx context.Context, ref ArchiveRef, targetPVC string) error
	// Delete removes the archive identified by ref.
	Delete(ctx context.Context, ref ArchiveRef) error
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
