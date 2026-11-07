package dbschema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
)

// Fingerprint hashes every regular file in fsys — names and contents, in
// lexical path order — into a hex string that changes whenever a migration is
// added, removed, renamed or edited. The stores expose theirs as
// MigrationFingerprint; internal/testdb keys its migrated template databases
// by it, so a template is rebuilt exactly when the migration set changes.
func Fingerprint(fsys fs.FS) string {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		body, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", path, len(body))
		_, _ = h.Write(body)
		return nil
	})
	if err != nil {
		// An embedded FS does not fail to read; if one ever does, no stable
		// fingerprint exists, so make that visible rather than reuse a stale
		// template.
		panic(fmt.Sprintf("dbschema: fingerprint migrations: %v", err))
	}
	return hex.EncodeToString(h.Sum(nil))
}
