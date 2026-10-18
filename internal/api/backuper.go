package api

import "context"

// Backuper returns after enqueueing; formerOwner controls later restore access and is empty for unowned worlds.
type Backuper interface {
	Backup(ctx context.Context, serverName, formerOwner string) error
}

// RestoreSnapshotter holds the world until a successful safety snapshot starts its restore or the chain is abandoned.
type RestoreSnapshotter interface {
	BackupThenRestore(ctx context.Context, serverName, formerOwner, backupID, backupRef string) error
}
