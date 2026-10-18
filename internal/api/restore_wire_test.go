package api

import (
	"felis.lolicon.best/internal/restore"
)

var _ Restorer = (*restore.Restorer)(nil)
