//go:build !linux

package main

import (
	"errors"
	"os"
)

// cloneFile has no reflink outside Linux; the copy falls back to bytes.
func cloneFile(_, _ *os.File) error { return errors.ErrUnsupported }
