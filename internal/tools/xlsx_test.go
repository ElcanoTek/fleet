//go:build fleet_host_executor

package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestXLSXWorkbookSetCellPreservesFormulaCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "book.xlsx")
	if err := writeXLSXToolTestWorkbook(path); err != nil {
		t.Fatalf("write workbook: %v", err)
	}

	if _, err := runXLSXAction(context.Background(), fsTestSandbox(t), XLSXParams{
		Action:    "set_cell",
		Path:      path,
		SheetName: "Data",
		Cell:      "C1",
		Value:     "Name",
	}); err != nil {
		t.Fatalf("set_cell: %v", err)
	}

	parts := readXLSXToolTestParts(t, path)
	sheet := parts["xl/worksheets/sheet1.xml"]
	if !strings.Contains(sheet, `<c r="A2"><f>1+1</f><v>2</v></c>`) {
		t.Fatalf("formula cache was not preserved: %s", sheet)
	}
	if !strings.Contains(sheet, `<c r="C1" t="inlineStr"><is><t>Name</t></is></c>`) {
		t.Fatalf("target cell was not updated: %s", sheet)
	}

	out, err := runXLSXAction(context.Background(), fsTestSandbox(t), XLSXParams{Action: "inspect", Path: path})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(out, `"workbook_xml_valid": true`) || !strings.Contains(out, `"cached_formula_cells": 1`) {
		t.Fatalf("unexpected inspect output: %s", out)
	}
}

func writeXLSXToolTestWorkbook(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := zip.NewWriter(f)
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Data" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Deal</t></is></c></row><row r="2"><c r="A2"><f>1+1</f><v>2</v></c></row></sheetData></worksheet>`,
	}
	for name, data := range parts {
		fw, err := w.Create(name)
		if err != nil {
			_ = w.Close()
			return err
		}
		if _, err := io.Copy(fw, bytes.NewBufferString(data)); err != nil {
			_ = w.Close()
			return err
		}
	}
	return w.Close()
}

func readXLSXToolTestParts(t *testing.T, path string) map[string]string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open workbook: %v", err)
	}
	defer r.Close()
	parts := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open part %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read part %s: %v", f.Name, err)
		}
		parts[f.Name] = string(data)
	}
	return parts
}

// zeroReader yields an endless stream of zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A model-supplied .xlsx is bounded only COMPRESSED. A ~600 MiB entry of
// zeros deflates to well under a megabyte; parsing must charge it against the
// decompressed budget and fail instead of inflating it whole. The entry is
// built streaming (never held in memory) and the budget is lowered through
// the parseXLSXPackageWithin seam, so the test reads ~1 MiB, not 512.
func TestParseXLSXPackage_DecompressedBudget(t *testing.T) {
	t.Parallel()

	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	fw, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.CopyN(fw, zeroReader{}, 600<<20); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if src.Len() > 4<<20 {
		t.Fatalf("bomb compressed to %d bytes; the test wants a highly compressible entry", src.Len())
	}

	_, err = parseXLSXPackageWithin(src.Bytes(), 1<<20)
	if !errors.Is(err, ErrXLSXTooLarge) {
		t.Fatalf("err = %v, want ErrXLSXTooLarge", err)
	}

	// The budget spans the whole workbook, not each part: two parts that
	// each fit alone fail together.
	var two bytes.Buffer
	zw = zip.NewWriter(&two)
	for _, name := range []string{"a.xml", "b.xml"} {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := io.CopyN(fw, zeroReader{}, 600<<10); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := parseXLSXPackageWithin(two.Bytes(), 1<<20); !errors.Is(err, ErrXLSXTooLarge) {
		t.Fatalf("two-part err = %v, want ErrXLSXTooLarge", err)
	}
	if _, err := parseXLSXPackageWithin(two.Bytes(), 2<<20); err != nil {
		t.Fatalf("within-budget parse: %v", err)
	}
}

// TestParseXLSXPackage_PartCapBeforeDirectoryParse proves the cap is applied
// to the raw bytes, before zip.NewReader builds the directory: an end record
// that understates the entry count does not change the outcome (with the old
// len(r.File) check this archive was fully parsed, then failed as ErrFormat).
func TestParseXLSXPackage_PartCapBeforeDirectoryParse(t *testing.T) {
	t.Parallel()

	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	for i := 0; i <= MaxXLSXParts; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: "p" + strconv.Itoa(i), Method: zip.Store}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw := src.Bytes()
	// Rewrite the end-of-central-directory entry counts to 1.
	eocd := bytes.LastIndex(raw, []byte{'P', 'K', 0x05, 0x06})
	if eocd < 0 {
		t.Fatal("no end record")
	}
	binary.LittleEndian.PutUint16(raw[eocd+8:], 1)
	binary.LittleEndian.PutUint16(raw[eocd+10:], 1)
	if _, err := parseXLSXPackage(raw); err == nil || !strings.Contains(err.Error(), "part cap") {
		t.Fatalf("err = %v, want the part-count cap", err)
	}
}

func TestParseXLSXPackage_PartCountCap(t *testing.T) {
	t.Parallel()

	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	for i := 0; i <= MaxXLSXParts; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: "p" + strconv.Itoa(i), Method: zip.Store}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := parseXLSXPackage(src.Bytes()); err == nil || !strings.Contains(err.Error(), "part cap") {
		t.Fatalf("err = %v, want the part-count cap", err)
	}
}
