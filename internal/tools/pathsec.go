package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathSecurityError represents a path security violation
type PathSecurityError struct {
	Path    string
	Reason  string
	BaseDir string
}

func (e *PathSecurityError) Error() string {
	return fmt.Sprintf("path security violation: %s (path: %s, allowed base: %s)", e.Reason, e.Path, e.BaseDir)
}

// AllowedBaseDirs returns the list of directories that file operations are allowed in.
// By default, this is the current working directory and the system temp directory (for testing).
// Additional directories can be specified via environment variable FLEET_ALLOWED_DIRS
// (or the legacy CHAT_ALLOWED_DIRS alias), colon-separated.
func AllowedBaseDirs() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current working directory: %w", err)
	}

	// Resolve to absolute path
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve working directory: %w", err)
	}

	dirs := []string{cwd}

	// Allow the system temp directory (needed for testing)
	tempDir := os.TempDir()
	if absTemp, err := filepath.Abs(tempDir); err == nil {
		dirs = append(dirs, absTemp)
	}

	// Allow additional directories via environment variable
	// (FLEET_ALLOWED_DIRS, or legacy CHAT_ALLOWED_DIRS).
	if extra := fleetEnv("ALLOWED_DIRS"); extra != "" {
		for _, dir := range strings.Split(extra, ":") {
			dir = strings.TrimSpace(dir)
			if dir == "" {
				continue
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				continue // Skip invalid paths
			}
			dirs = append(dirs, absDir)
		}
	}

	return dirs, nil
}

// ValidatePath checks if the given path is within allowed directories.
// It resolves the path to an absolute path and checks for path traversal attempts.
// Returns the resolved absolute path if valid, or an error if the path is not allowed.
func ValidatePath(path string) (string, error) {
	if path == "" {
		return "", &PathSecurityError{Path: path, Reason: "empty path", BaseDir: ""}
	}

	// Get allowed base directories
	allowedDirs, err := AllowedBaseDirs()
	if err != nil {
		return "", err
	}

	// Resolve the path to absolute
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", &PathSecurityError{Path: path, Reason: "cannot resolve path", BaseDir: strings.Join(allowedDirs, ":")}
	}

	// Clean the path to resolve . and ..
	absPath = filepath.Clean(absPath)

	// Check if the path is within any allowed directory
	for _, baseDir := range allowedDirs {
		baseDir = filepath.Clean(baseDir)

		// Check if absPath starts with baseDir
		// We need to be careful here: /home/user/test should match /home/user
		// but /home/username should not match /home/user
		if isSubPath(baseDir, absPath) {
			// Additional check: if the file exists, resolve symlinks and check again
			if realPath, err := filepath.EvalSymlinks(absPath); err == nil {
				// The file exists, check the real path too
				if !isSubPathAny(allowedDirs, realPath) {
					return "", &PathSecurityError{
						Path:    path,
						Reason:  "symlink points outside allowed directories",
						BaseDir: strings.Join(allowedDirs, ":"),
					}
				}
				return realPath, nil
			}
			// File doesn't exist yet, that's okay for write operations
			// But we must verify that the closest existing parent directory is within bounds
			checkPath := absPath
			for {
				parent := filepath.Dir(checkPath)
				// If we hit the root or same dir (e.g. /), break out
				if parent == checkPath || parent == "" {
					break
				}
				checkPath = parent

				if realParent, err := filepath.EvalSymlinks(checkPath); err == nil {
					if !isSubPathAny(allowedDirs, realParent) {
						return "", &PathSecurityError{
							Path:    path,
							Reason:  "parent directory symlink points outside allowed directories",
							BaseDir: strings.Join(allowedDirs, ":"),
						}
					}
					break // Closest existing parent is safe
				} else if !os.IsNotExist(err) {
					// Some other error (e.g., permission denied), play it safe
					return "", &PathSecurityError{
						Path:    path,
						Reason:  fmt.Sprintf("cannot evaluate parent directory: %v", err),
						BaseDir: strings.Join(allowedDirs, ":"),
					}
				}
				// If it does not exist, continue loop to check the next parent up
			}
			return absPath, nil
		}
	}

	return "", &PathSecurityError{
		Path:    path,
		Reason:  "path is outside allowed directories",
		BaseDir: strings.Join(allowedDirs, ":"),
	}
}

// ValidatePathForRead validates a path for read operations.
// The file must exist and be within allowed directories.
func ValidatePathForRead(path string) (string, error) {
	absPath, err := ValidatePath(path)
	if err != nil {
		return "", err
	}

	// Check file exists
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file does not exist: %s", path)
		}
		return "", fmt.Errorf("cannot access file: %w", err)
	}

	// Don't allow reading directories as files
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s", path)
	}

	return absPath, nil
}

// ValidateDirectory validates that a directory path is within allowed directories.
func ValidateDirectory(path string) (string, error) {
	if path == "" {
		// Empty path means current directory, which is always allowed
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return cwd, nil
	}

	absPath, err := ValidatePath(path)
	if err != nil {
		return "", err
	}

	// Check it's a directory if it exists
	info, err := os.Stat(absPath)
	if err == nil && !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", path)
	}

	return absPath, nil
}

// isSubPath checks if child is a subpath of parent
func isSubPath(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)

	// Exact match
	if parent == child {
		return true
	}

	// Check if child starts with parent + separator
	parentWithSep := parent + string(filepath.Separator)
	return strings.HasPrefix(child, parentWithSep)
}

// isSubPathAny checks if path is a subpath of any of the allowed directories
func isSubPathAny(allowedDirs []string, path string) bool {
	for _, dir := range allowedDirs {
		// Resolve symlinks in the allowed directory for comparison
		// This is needed on macOS where /var is a symlink to /private/var
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			resolvedDir = dir
		}
		if isSubPath(resolvedDir, path) || isSubPath(dir, path) {
			return true
		}
	}
	return false
}
