//go:build !linux

package fileedit

import (
	"errors"
	"os"
)

func renameNoReplace(_ *os.Root, _, _ string) error {
	return errors.New("fileedit: atomic file placement requires Linux")
}
