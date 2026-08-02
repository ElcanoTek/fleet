package httpapi

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var (
	workbookIgnorableAttrRe    = regexp.MustCompile(`\s+mc:Ignorable="[^"]*"`)
	workbookAlternateContentRe = regexp.MustCompile(`(?s)<mc:AlternateContent\b.*?</mc:AlternateContent>`)
	workbookRevisionPtrRe      = regexp.MustCompile(`(?s)<xr:revisionPtr\b[^>]*/>`)
	workbookExtensionListRe    = regexp.MustCompile(`(?s)<extLst\b.*?</extLst>`)
)

func sanitizeXLSX(path string) error {
	return sanitizeXLSXPackage(path)
}

func sanitizeXLSXPackage(path string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "xlsx-sanitize-*.xlsx")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	w := zip.NewWriter(tmp)
	// The upload path bounds only the COMPRESSED size; deflate expands up to
	// ~1000x, so an unbounded rewrite would let a small "xlsx" zip bomb OOM
	// the process (workbook.xml is read into memory) or fill the disk (other
	// parts stream to the temp file). One decompressed-bytes budget across
	// all parts bounds both.
	budget := int64(maxXLSXDecompressedBytes)
	for _, f := range r.File {
		if err := copyXLSXPart(w, f, &budget); err != nil {
			_ = w.Close()
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// maxXLSXDecompressedBytes is the total decompressed budget for one workbook's
// parts. Each part is held in memory while it is rewritten, so this bound is
// also the peak-RSS bound for a hostile file. Real spreadsheets are megabytes
// decompressed; 512 MiB is far above any legitimate workbook while keeping a
// zip bomb to a survivable allocation.
const maxXLSXDecompressedBytes = 512 << 20

func copyXLSXPart(w *zip.Writer, f *zip.File, budget *int64) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()

	// +1 so exhausting the budget is distinguishable from landing exactly on it.
	limited := io.LimitReader(rc, *budget+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read %s: %w", f.Name, err)
	}
	*budget -= int64(len(data))
	if *budget < 0 {
		return fmt.Errorf("read %s: workbook decompresses past the %d-byte cap", f.Name, int64(maxXLSXDecompressedBytes))
	}

	if filepath.ToSlash(f.Name) == "xl/workbook.xml" {
		data = stripWorkbookMetadata(data)
	}

	hdr := &zip.FileHeader{
		Name:           f.Name,
		Method:         f.Method,
		Modified:       f.Modified,
		ModifiedTime:   f.ModifiedTime,
		ModifiedDate:   f.ModifiedDate,
		NonUTF8:        f.NonUTF8,
		CreatorVersion: f.CreatorVersion,
		ReaderVersion:  f.ReaderVersion,
		Flags:          f.Flags,
		ExternalAttrs:  f.ExternalAttrs,
		Extra:          f.Extra,
		Comment:        f.Comment,
	}
	if hdr.Modified.IsZero() {
		hdr.Modified = time.Now()
	}
	wc, err := w.CreateHeader(hdr)
	if err != nil {
		return fmt.Errorf("create %s: %w", f.Name, err)
	}
	if _, err := wc.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", f.Name, err)
	}
	return nil
}

func stripWorkbookMetadata(data []byte) []byte {
	s := string(data)
	s = workbookIgnorableAttrRe.ReplaceAllString(s, "")
	s = workbookAlternateContentRe.ReplaceAllString(s, "")
	s = workbookRevisionPtrRe.ReplaceAllString(s, "")
	s = workbookExtensionListRe.ReplaceAllString(s, "")
	return []byte(s)
}
