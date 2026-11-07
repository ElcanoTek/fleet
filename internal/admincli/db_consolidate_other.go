// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

//go:build !linux

package admincli

import "errors"

// freeBytesAt is unmeasured off Linux; the disk preflight then warns.
func freeBytesAt(string) (uint64, error) {
	return 0, errors.New("not measured on this platform")
}
