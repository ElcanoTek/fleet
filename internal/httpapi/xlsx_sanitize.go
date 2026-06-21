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
	for _, f := range r.File {
		if err := copyXLSXPart(w, f); err != nil {
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

func copyXLSXPart(w *zip.Writer, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read %s: %w", f.Name, err)
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
