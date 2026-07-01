package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fingerprintEntries are the quality-relevant bundle content an eval replay
// depends on: the manifest (models/providers/personas policy), the prompt
// sources, and the eval definitions themselves. Deliberately EXCLUDED: mcp/
// server code and sandbox/ Containerfile — they affect capability, not the
// prompt-level behavior goldens compare, and hashing a whole vendored server
// tree would churn the fingerprint on irrelevant edits.
var fingerprintEntries = []string{
	"manifest.yaml",
	"system_prompts",
	"personas",
	"protocols",
	"skills",
	"evals",
}

// BundleFingerprint returns a stable content hash ("sha256:<hex>") of the
// bundle's quality-relevant files, recorded on every eval_runs row so two runs
// are comparable only when they replayed the same bundle content. The RAW file
// bytes are hashed (pre-env-interpolation for the manifest), so secrets
// resolved from the environment never influence — or leak into — the
// fingerprint.
func BundleFingerprint(bundleDir string) (string, error) {
	h := sha256.New()
	for _, entry := range fingerprintEntries {
		root := filepath.Join(bundleDir, entry)
		info, err := os.Stat(root)
		if err != nil {
			continue // optional bundle content — absent dirs simply don't contribute
		}
		if !info.IsDir() {
			if err := hashFile(h, root, entry); err != nil {
				return "", err
			}
			continue
		}
		var files []string
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", entry, err)
		}
		sort.Strings(files)
		for _, f := range files {
			rel, err := filepath.Rel(bundleDir, f)
			if err != nil {
				return "", err
			}
			if err := hashFile(h, f, filepath.ToSlash(rel)); err != nil {
				return "", err
			}
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// hashFile mixes the file's bundle-relative path and bytes into h, with NUL
// separators so path/content boundaries can't alias.
func hashFile(h interface{ Write([]byte) (int, error) }, path, rel string) error {
	raw, err := os.ReadFile(path) // #nosec G304 — operator-supplied bundle path.
	if err != nil {
		return fmt.Errorf("fingerprint read %s: %w", rel, err)
	}
	if _, err := h.Write([]byte(strings.ToLower(rel))); err != nil {
		return err
	}
	if _, err := h.Write([]byte{0}); err != nil {
		return err
	}
	if _, err := h.Write(raw); err != nil {
		return err
	}
	_, err = h.Write([]byte{0})
	return err
}
