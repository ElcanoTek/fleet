// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package accountdelete

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// heldPrefix names a tree a deletion holds (holdAside): the deletion did not
// commit and the account could not be read back, so whether the tree belongs
// to an account still there is not known. The name records the address's
// digest and the account's id: .held-<digest>-<hex of the id>-<random>. No
// sweep removes a held tree as it removes the asidePrefix ones; the
// maintenance pass resolves it (ResolveHeldIn).
const heldPrefix = ".held-"

// holdRename is the rename holdAside makes; a test makes it fail.
var holdRename = os.Rename

// holdTries is how many held names holdAll tries for one tree.
const holdTries = 3

// holdAside renames t's set-aside tree to a held name beside it, recording
// digest and accountID.
func holdAside(t AsideTree, digest, accountID string) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	name := heldPrefix + digest + "-" + hex.EncodeToString([]byte(accountID)) + "-" + hex.EncodeToString(suffix[:])
	return holdRename(t.To, filepath.Join(filepath.Dir(t.To), name))
}

// holdAll holds every tree in aside (holdAside), trying holdTries names for
// each and going on to the next tree when one fails. A tree that cannot be
// held is put back where it was rather than left under its set-aside name,
// which the sweeps remove whatever the account (only a tree that may go is
// ever left under that name). It returns one error for each tree that could
// not be held, saying where it is.
func holdAll(aside []AsideTree, digest, accountID string) error {
	var errs []error
	for _, t := range aside {
		var err error
		for try := 0; try < holdTries; try++ {
			if err = holdAside(t, digest, accountID); err == nil {
				break
			}
		}
		if err == nil {
			continue
		}
		if rerr := os.Rename(t.To, t.From); rerr != nil {
			errs = append(errs, fmt.Errorf("%s could not be held (%w) nor put back (%w); it is at %s", t.From, err, rerr, t.To))
			continue
		}
		errs = append(errs, fmt.Errorf("%s could not be held (%w), so it was put back", t.From, err))
	}
	return errors.Join(errs...)
}

// IsHeld reports whether name is a tree a deletion holds (holdAside).
func IsHeld(name string) bool {
	_, _, ok := parseHeld(name)
	return ok
}

// parseHeld reads the digest and account id a held tree's name records.
func parseHeld(name string) (digest, accountID string, ok bool) {
	rest, found := strings.CutPrefix(name, heldPrefix)
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "-")
	if len(parts) != 3 || !IsDigest(parts[0]) {
		return "", "", false
	}
	id, err := hex.DecodeString(parts[1])
	if err != nil || len(id) == 0 {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil || parts[2] == "" {
		return "", "", false
	}
	return parts[0], string(id), true
}

// HeldStats counts what ResolveHeldIn did with the held trees it looked at.
type HeldStats struct {
	// Restored trees went back to the address's path whole, or had all their
	// entries moved into it.
	Restored int
	// Removed trees belonged to an account that is gone.
	Removed int
	// Left trees stay held: the account could not be read, it now has
	// another address, or some entries could not be moved back.
	Left int
}

// ErrAccountBusy is what ResolveHeldIn's withAccount answers while a
// deletion of the tree's account runs: the tree waits for a later pass.
var ErrAccountBusy = errors.New("a deletion of the account is running")

// ErrHeldEntriesLeft reports a held tree whose account is still there but
// some of whose entries are left held: the address's path already has an
// entry of that name (written since), which is never overwritten.
var ErrHeldEntriesLeft = errors.New("entries the account has written again since are left held")

// ResolveHeldIn resolves the trees held in dir (holdAside), at most *budget
// of them, decrementing it. For each it calls withAccount with the account id
// the name records; withAccount runs its function with that account's
// address while the account is held still (no deletion of it can run until
// the function returns: the server claims the account and locks its row,
// store.Store.WithAccountRowByID), and reports found false when no account
// has that id:
//   - the account is there under an address with the tree's digest: the tree
//     goes back to dir/<digest>, inside that function. When that path exists
//     already (the account wrote files since), the held tree's top-level
//     entries it does not have are moved into it; an entry it has is never
//     overwritten, and stays held (ErrHeldEntriesLeft);
//   - no account has that id (the deletion did commit, whatever account holds
//     the address now): the tree is removed;
//   - the account is there under another address, a deletion of it is
//     running (ErrAccountBusy), or the read fails: the tree stays held for a
//     later pass.
//
// It returns what it did and the first failure. A missing dir has none.
func ResolveHeldIn(dir string, withAccount func(accountID string, fn func(email string) error) (found bool, err error), budget *int) (HeldStats, error) {
	var st HeldStats
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	var firstErr error
	for _, e := range entries {
		digest, id, ok := parseHeld(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		if *budget <= 0 {
			break
		}
		*budget--
		held := filepath.Join(dir, e.Name())
		otherAddress := false
		found, err := withAccount(id, func(email string) error {
			if Digest(email) != digest {
				otherAddress = true
				return nil
			}
			return restoreHeld(held, filepath.Join(dir, digest))
		})
		switch {
		case errors.Is(err, ErrAccountBusy):
			st.Left++
		case err != nil:
			st.Left++
			firstErr = firstError(firstErr, err)
		case !found:
			if err := os.RemoveAll(held); err != nil {
				st.Left++
				firstErr = firstError(firstErr, err)
				continue
			}
			st.Removed++
		case otherAddress:
			st.Left++
		default:
			st.Restored++
		}
	}
	return st, firstErr
}

// restoreHeld puts the held tree back at path: renamed into place when path
// does not exist, otherwise entry by entry (mergeHeld).
func restoreHeld(held, path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := os.Rename(held, path); err == nil {
			return nil
		}
		// Written in the meantime: merge instead.
	} else if err != nil {
		return err
	}
	return mergeHeld(held, path)
}

// mergeHeld moves each top-level entry of held that path does not have into
// path, then removes held if that emptied it. An entry path has already is
// left in held (ErrHeldEntriesLeft); path not being a directory leaves all.
func mergeHeld(held, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %w", path, ErrHeldEntriesLeft)
	}
	entries, err := os.ReadDir(held)
	if err != nil {
		return err
	}
	left := false
	for _, e := range entries {
		dst := filepath.Join(path, e.Name())
		if _, err := os.Lstat(dst); !os.IsNotExist(err) {
			left = true
			continue
		}
		if err := os.Rename(filepath.Join(held, e.Name()), dst); err != nil {
			return err
		}
	}
	if left {
		return ErrHeldEntriesLeft
	}
	return os.Remove(held)
}
