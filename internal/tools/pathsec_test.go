package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSubPath(t *testing.T) {
	tests := []struct {
		name     string
		parent   string
		child    string
		expected bool
	}{
		{"exact match", "/home/user", "/home/user", true},
		{"valid subpath", "/home/user", "/home/user/docs/file.txt", true},
		{"valid subpath with trailing slash", "/home/user/", "/home/user/docs", true},
		{"prefix match not subpath", "/home/user", "/home/username/file.txt", false},
		{"completely different", "/home/user", "/etc/passwd", false},
		{"parent is root", "/", "/home/user", false},
		{"child is root", "/home/user", "/", false},
		{"relative paths exact match", "docs", "docs", true},
		{"relative paths subpath", "docs", "docs/file.txt", true},
		{"relative paths prefix match", "docs", "docs_old/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSubPath(tt.parent, tt.child)
			if result != tt.expected {
				t.Errorf("isSubPath(%q, %q) = %v; expected %v", tt.parent, tt.child, result, tt.expected)
			}
		})
	}
}

func TestIsSubPathAny(t *testing.T) {
	allowedDirs := []string{
		"/home/user",
		"/tmp/testdir",
		"/var/log",
	}

	tests := []struct {
		name        string
		allowedDirs []string
		path        string
		expected    bool
	}{
		{"match first dir", allowedDirs, "/home/user/docs/file.txt", true},
		{"match middle dir", allowedDirs, "/tmp/testdir/file.txt", true},
		{"match last dir", allowedDirs, "/var/log/syslog", true},
		{"no match", allowedDirs, "/etc/passwd", false},
		{"prefix match not subpath", allowedDirs, "/home/username/file.txt", false},
		{"empty allowed dirs", []string{}, "/home/user/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSubPathAny(tt.allowedDirs, tt.path)
			if result != tt.expected {
				t.Errorf("isSubPathAny() = %v; expected %v", result, tt.expected)
			}
		})
	}

	t.Run("symlink in allowed dir", func(t *testing.T) {
		// Create a temporary directory structure to test symlinks
		tempDir, err := os.MkdirTemp("", "pathsec-test-*")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tempDir)

		realDir := filepath.Join(tempDir, "real")
		err = os.MkdirAll(realDir, 0755)
		if err != nil {
			t.Fatalf("failed to create real dir: %v", err)
		}

		symlinkDir := filepath.Join(tempDir, "symlink")
		err = os.Symlink(realDir, symlinkDir)
		if err != nil {
			t.Fatalf("failed to create symlink: %v", err)
		}

		// The path we want to check is inside the real directory
		testPath := filepath.Join(realDir, "file.txt")

		// If we allow the symlink directory, the test path (which is the real path) should be allowed
		// isSubPathAny resolves symlinks in the allowed directories
		allowed := []string{symlinkDir}

		if !isSubPathAny(allowed, testPath) {
			t.Errorf("isSubPathAny() with symlink in allowed dir failed; expected true")
		}
	})
}

func TestAllowedBaseDirs(t *testing.T) {
	cwd, _ := os.Getwd()
	absCwd, _ := filepath.Abs(cwd)
	tempDir := os.TempDir()
	absTemp, _ := filepath.Abs(tempDir)

	tests := []struct {
		name         string
		envVal       string
		expectedDirs []string
	}{
		{
			"default without env",
			"",
			[]string{absCwd, absTemp},
		},
		{
			"with one extra valid dir",
			"/var/log",
			[]string{absCwd, absTemp, "/var/log"},
		},
		{
			"with multiple extra valid dirs",
			"/var/log:/tmp/testdir",
			[]string{absCwd, absTemp, "/var/log", "/tmp/testdir"},
		},
		{
			"with extra dirs containing spaces",
			" /var/log : /tmp/testdir ",
			[]string{absCwd, absTemp, "/var/log", "/tmp/testdir"},
		},
		{
			"with empty extra dirs",
			":::",
			[]string{absCwd, absTemp},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FLEET_ALLOWED_DIRS", tt.envVal)

			dirs, err := AllowedBaseDirs()
			if err != nil {
				t.Fatalf("AllowedBaseDirs() returned unexpected error: %v", err)
			}

			// We need to resolve the expected extra dirs just like AllowedBaseDirs does
			resolvedExpected := []string{absCwd, absTemp}
			if tt.envVal != "" {
				for _, d := range strings.Split(tt.envVal, ":") {
					d = strings.TrimSpace(d)
					if d == "" {
						continue
					}
					if absD, err := filepath.Abs(d); err == nil {
						resolvedExpected = append(resolvedExpected, absD)
					}
				}
			}

			if len(dirs) != len(resolvedExpected) {
				t.Errorf("AllowedBaseDirs() returned %d dirs, expected %d", len(dirs), len(resolvedExpected))
			}

			for i, d := range dirs {
				if d != resolvedExpected[i] {
					t.Errorf("AllowedBaseDirs() returned dir[%d] = %q, expected %q", i, d, resolvedExpected[i])
				}
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	cwd, _ := os.Getwd()
	absCwd, _ := filepath.Abs(cwd)
	tempDir := os.TempDir()
	absTemp, _ := filepath.Abs(tempDir)

	tests := []struct {
		name          string
		path          string
		expectedError string
		expectedPath  string
	}{
		{"empty path", "", "empty path", ""},
		{"valid path in cwd", filepath.Join(absCwd, "test.txt"), "", filepath.Join(absCwd, "test.txt")},
		{"valid path in temp", filepath.Join(absTemp, "test.txt"), "", filepath.Join(absTemp, "test.txt")},
		{"relative path in cwd", "test.txt", "", filepath.Join(absCwd, "test.txt")},
		{"path outside allowed", "/etc/passwd", "path is outside allowed directories", ""},
		{"path traversal attempt escaping cwd", "../../../../etc/passwd", "path is outside allowed directories", ""},
		{"path traversal staying inside cwd", filepath.Join(absCwd, "dir", "..", "test.txt"), "", filepath.Join(absCwd, "test.txt")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidatePath(tt.path)

			if tt.expectedError != "" {
				if err == nil {
					t.Errorf("ValidatePath() expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("ValidatePath() expected error containing %q, got %q", tt.expectedError, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("ValidatePath() unexpected error: %v", err)
				}
				if result != tt.expectedPath {
					t.Errorf("ValidatePath() returned %q, expected %q", result, tt.expectedPath)
				}
			}
		})
	}

	t.Run("symlink pointing outside", func(t *testing.T) {
		tempBase, err := os.MkdirTemp("", "pathsec-test-*")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tempBase)

		// Create an allowed dir
		allowedDir := filepath.Join(tempBase, "allowed")
		err = os.MkdirAll(allowedDir, 0755)
		if err != nil {
			t.Fatalf("failed to create allowed dir: %v", err)
		}

		t.Setenv("FLEET_ALLOWED_DIRS", allowedDir)

		// Create a symlink pointing to an absolute path known to be outside (e.g. /etc/passwd or /var/log)
		// os.TempDir is automatically allowed, so we cannot just point inside tempBase.
		symlinkPath := filepath.Join(allowedDir, "symlink.txt")
		err = os.Symlink("/etc/passwd", symlinkPath)
		if err != nil {
			t.Fatalf("failed to create symlink: %v", err)
		}

		// Try to validate the symlink
		_, err = ValidatePath(symlinkPath)
		if err == nil {
			t.Errorf("ValidatePath() expected error for symlink pointing outside, got nil")
		} else if !strings.Contains(err.Error(), "symlink points outside allowed directories") {
			t.Errorf("ValidatePath() expected error containing 'symlink points outside', got %q", err.Error())
		}
	})

	t.Run("symlink parent pointing outside", func(t *testing.T) {
		tempBase, err := os.MkdirTemp("", "pathsec-test-*")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tempBase)

		allowedDir := filepath.Join(tempBase, "allowed")
		err = os.MkdirAll(allowedDir, 0755)
		if err != nil {
			t.Fatalf("failed to create allowed dir: %v", err)
		}

		t.Setenv("FLEET_ALLOWED_DIRS", allowedDir)

		// Parent dir is a symlink to /etc (or similar outside path)
		symlinkDir := filepath.Join(allowedDir, "symlink_dir")
		err = os.Symlink("/etc", symlinkDir)
		if err != nil {
			t.Fatalf("failed to create symlink dir: %v", err)
		}

		// The path we want to check uses the symlink directory
		testPath := filepath.Join(symlinkDir, "new_file.txt")

		_, err = ValidatePath(testPath)
		if err == nil {
			t.Errorf("ValidatePath() expected error for parent symlink pointing outside, got nil")
		} else if !strings.Contains(err.Error(), "parent directory symlink points outside allowed directories") {
			t.Errorf("ValidatePath() expected error containing 'parent directory symlink points outside', got %q", err.Error())
		}
	})
}

func TestValidatePathForRead(t *testing.T) {
	tempBase, err := os.MkdirTemp("", "pathsec-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempBase)

	// Create a valid file
	validFile := filepath.Join(tempBase, "valid.txt")
	err = os.WriteFile(validFile, []byte("test content"), 0644)
	if err != nil {
		t.Fatalf("failed to create valid file: %v", err)
	}

	// Create a valid directory
	validDir := filepath.Join(tempBase, "validdir")
	err = os.MkdirAll(validDir, 0755)
	if err != nil {
		t.Fatalf("failed to create valid dir: %v", err)
	}

	tests := []struct {
		name          string
		path          string
		expectedError string
	}{
		{"existing valid file", validFile, ""},
		{"non-existent file", filepath.Join(tempBase, "does_not_exist.txt"), "file does not exist"},
		{"directory instead of file", validDir, "path is a directory, not a file"},
		{"path outside allowed directories", "/etc/passwd", "path is outside allowed directories"}, // Reuses ValidatePath internally
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidatePathForRead(tt.path)

			if tt.expectedError != "" {
				if err == nil {
					t.Errorf("ValidatePathForRead() expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("ValidatePathForRead() expected error containing %q, got %q", tt.expectedError, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("ValidatePathForRead() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateDirectory(t *testing.T) {
	cwd, _ := os.Getwd()
	absCwd, _ := filepath.Abs(cwd)

	tempBase, err := os.MkdirTemp("", "pathsec-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempBase)

	// Create a valid directory
	validDir := filepath.Join(tempBase, "validdir")
	err = os.MkdirAll(validDir, 0755)
	if err != nil {
		t.Fatalf("failed to create valid dir: %v", err)
	}

	// Create a valid file
	validFile := filepath.Join(tempBase, "valid.txt")
	err = os.WriteFile(validFile, []byte("test content"), 0644)
	if err != nil {
		t.Fatalf("failed to create valid file: %v", err)
	}

	tests := []struct {
		name          string
		path          string
		expectedError string
		expectedPath  string
	}{
		{"empty path returns cwd", "", "", absCwd},
		{"valid directory", validDir, "", validDir},
		{"file instead of directory", validFile, "path is not a directory", ""},
		{"non-existent directory within allowed", filepath.Join(tempBase, "newdir"), "", filepath.Join(tempBase, "newdir")},
		{"directory outside allowed", "/etc", "path is outside allowed directories", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidateDirectory(tt.path)

			if tt.expectedError != "" {
				if err == nil {
					t.Errorf("ValidateDirectory() expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("ValidateDirectory() expected error containing %q, got %q", tt.expectedError, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("ValidateDirectory() unexpected error: %v", err)
				}
				if result != tt.expectedPath {
					t.Errorf("ValidateDirectory() returned %q, expected %q", result, tt.expectedPath)
				}
			}
		})
	}
}

func TestValidatePathExploitDeepSubdir(t *testing.T) {
	tempBase, err := os.MkdirTemp("", "pathsec-exploit-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempBase)

	// Create an allowed dir
	allowedDir := filepath.Join(tempBase, "allowed")
	err = os.MkdirAll(allowedDir, 0755)
	if err != nil {
		t.Fatalf("failed to create allowed dir: %v", err)
	}

	t.Setenv("FLEET_ALLOWED_DIRS", allowedDir)

	// Parent dir is a symlink to an outside directory
	symlinkDir := filepath.Join(allowedDir, "symlink_dir")
	// Use something not in AllowedBaseDirs
	err = os.Symlink("/var/tmp", symlinkDir)
	if err != nil {
		t.Fatalf("failed to create symlink dir: %v", err)
	}

	// We pass a path that traverses through the symlink to a nonexistent subdirectory!
	testPath := filepath.Join(symlinkDir, "non_existent_subdir", "new_file.txt")

	_, err = ValidatePath(testPath)
	if err == nil {
		t.Fatalf("VULNERABILITY DETECTED! Returned allowed path for symlink bypass via nonexistent subdir")
	} else if !strings.Contains(err.Error(), "parent directory symlink points outside allowed directories") {
		t.Errorf("ValidatePath() expected error containing 'parent directory symlink points outside allowed directories', got %q", err.Error())
	}
}
