package api

import (
	"felis.lolicon.best/internal/backupjob"
)

// Compile-time proof that the production backup executor satisfies the API's
// Backuper interface, mirroring restore_wire_test. The dependency points one way
// only: api defines the narrow Backuper port and never imports backupjob in
// production (handlers_backups.go depends on the interface); this test is the single
// place the concrete type and the port are pinned together.
var _ Backuper = (*backupjob.Backuper)(nil)
