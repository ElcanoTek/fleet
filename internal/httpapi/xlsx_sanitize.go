package httpapi

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ElcanoTek/fleet/internal/tools"
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
	budget := int64(tools.MaxXLSXDecompressedBytes)
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

func copyXLSXPart(w *zip.Writer, f *zip.File, budget *int64) error {
	data, err := tools.ReadXLSXPart(f, budget)
	if err != nil {
		return err
	}

	if filepath.ToSlash(f.Name) == "xl/workbook.xml" {
		data = stripWorkbookMetadata(data)
	}

	// ModifiedTime/ModifiedDate are deliberately NOT copied. They are the
	// deprecated MS-DOS timestamp pair, and archive/zip's writer recomputes
	// both from Modified whenever Modified is non-zero
	// (writer.go: "If Modified is set, this takes precedence over MS-DOS
	// timestamp fields"). The Modified fallback below guarantees it is never
	// zero, so copying the pair only fed values the writer immediately
	// overwrote — dropping them is byte-for-byte identical output.
	hdr := &zip.FileHeader{
		Name:           f.Name,
		Method:         f.Method,
		Modified:       f.Modified,
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
