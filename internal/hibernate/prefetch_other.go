//go:build !linux

package hibernate

import "errors"

// PrefetchSwap needs process_madvise, which only Linux has.
func PrefetchSwap(int) error { return errors.New("hibernation needs Linux") }
