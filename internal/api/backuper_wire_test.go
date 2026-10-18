package api

import (
	"felis.lolicon.best/internal/backupjob"
)

var _ Backuper = (*backupjob.Backuper)(nil)
