package api

import (
	"felis.lolicon.best/internal/restore"
)

// Compile-time proof that the production restore executor satisfies the API's
// Restorer interface. The dependency points one way only: api defines the
// narrow Restorer port and never imports the restore package in production
// (handlers_backups.go depends on the interface); this test is the single place
// the concrete type and the port are pinned together, mirroring how images_test
// pins build.Builder to ImageBuilder.
var _ Restorer = (*restore.Restorer)(nil)
