package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkGetFileChecksums(b *testing.B) {
	// Setup temp dir
	tmpDir := b.TempDir()
	// Create temp_uploads dir
	uploadsDir := filepath.Join(tmpDir, "temp_uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		b.Fatal(err)
	}

	// Create dummy files
	numFiles := 50
	files := make([]string, numFiles)
	content := make([]byte, 1024*10) // 10KB
	for i := range content {
		content[i] = byte(i % 256)
	}

	for i := 0; i < numFiles; i++ {
		filename := fmt.Sprintf("testfile_%d.txt", i)
		if err := os.WriteFile(filepath.Join(uploadsDir, filename), content, 0644); err != nil {
			b.Fatal(err)
		}
		files[i] = filename
	}

	h := &Handlers{
		config:        Config{DataDir: tmpDir},
		checksumCache: newChecksumCache(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Clear memory cache
		h.checksumCache.Clear()
		// Clear sidecar files to force recalculation
		os.RemoveAll(filepath.Join(uploadsDir, ".checksums"))
		b.StartTimer()

		h.getFileChecksums(files)
	}
}

func BenchmarkGetFileChecksums_Duplicates(b *testing.B) {
	// Setup temp dir
	tmpDir := b.TempDir()
	// Create temp_uploads dir
	uploadsDir := filepath.Join(tmpDir, "temp_uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		b.Fatal(err)
	}

	// Create dummy files - 5 unique files
	numUniqueFiles := 5
	uniqueFiles := make([]string, numUniqueFiles)
	content := make([]byte, 1024*10) // 10KB
	for i := range content {
		content[i] = byte(i % 256)
	}

	for i := 0; i < numUniqueFiles; i++ {
		filename := fmt.Sprintf("testfile_dup_%d.txt", i)
		if err := os.WriteFile(filepath.Join(uploadsDir, filename), content, 0644); err != nil {
			b.Fatal(err)
		}
		uniqueFiles[i] = filename
	}

	// Create list with many duplicates - e.g. 500 items total
	totalFiles := 500
	files := make([]string, totalFiles)
	for i := 0; i < totalFiles; i++ {
		files[i] = uniqueFiles[i%numUniqueFiles]
	}

	h := &Handlers{
		config:        Config{DataDir: tmpDir},
		checksumCache: newChecksumCache(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Clear memory cache
		h.checksumCache.Clear()
		// Clear sidecar files to force recalculation
		os.RemoveAll(filepath.Join(uploadsDir, ".checksums"))
		b.StartTimer()

		h.getFileChecksums(files)
	}
}
