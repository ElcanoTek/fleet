package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeXLSXPackage_PreservesCachedFormulaValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.xlsx")
	if err := writeTestWorkbook(path); err != nil {
		t.Fatalf("write workbook: %v", err)
	}

	if err := sanitizeXLSXPackage(path); err != nil {
		t.Fatalf("sanitize workbook: %v", err)
	}

	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open sanitized workbook: %v", err)
	}
	defer r.Close()

	parts := map[string]string{}
	for _, f := range r.File {
		data, err := readZipPart(f)
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		parts[f.Name] = data
	}

	if _, ok := parts["xl/metadata.xml"]; !ok {
		t.Fatal("metadata part was unexpectedly removed")
	}
	if _, ok := parts["xl/calcChain.xml"]; !ok {
		t.Fatal("calcChain part was unexpectedly removed")
	}
	if strings.Contains(parts["xl/workbook.xml"], "revisionPtr") {
		t.Fatal("workbook revision metadata still present")
	}
	if strings.Contains(parts["xl/workbook.xml"], "AlternateContent") {
		t.Fatal("workbook alternate content still present")
	}
	if strings.Contains(parts["xl/workbook.xml"], "extLst") {
		t.Fatal("workbook extension list still present")
	}
	if err := xml.Unmarshal([]byte(parts["xl/workbook.xml"]), new(any)); err != nil {
		t.Fatalf("workbook XML is invalid: %v", err)
	}
	if !strings.Contains(parts["xl/workbook.xml"], "Tracking_US") {
		t.Fatal("worksheet data was unexpectedly removed")
	}
	if !strings.Contains(parts["xl/worksheets/sheet1.xml"], "<v>42</v>") {
		t.Fatal("worksheet cell data was unexpectedly removed")
	}
}

func writeTestWorkbook(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
  <Override PartName="/xl/metadata.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheetMetadata+xml"/>
  <Override PartName="/xl/calcChain.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.calcChain+xml"/>
</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006" mc:Ignorable="x15 xr" xmlns:x15="http://schemas.microsoft.com/office/spreadsheetml/2010/11/main" xmlns:xr="http://schemas.microsoft.com/office/spreadsheetml/2014/revision">
  <mc:AlternateContent><mc:Choice Requires="x15"><x15:workbookPr/></mc:Choice></mc:AlternateContent>
  <xr:revisionPtr revIDLastSave="1" documentId="doc"/>
  <bookViews><workbookView xWindow="0" yWindow="0" windowWidth="100" windowHeight="100"/></bookViews>
  <sheets><sheet name="Tracking_US" sheetId="1" r:id="rId1"/></sheets>
  <extLst><ext uri="test"/></extLst>
</workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sheetMetadata" Target="metadata.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/calcChain" Target="calcChain.xml"/>
</Relationships>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData><row r="1"><c r="A1"><v>42</v></c></row></sheetData>
</worksheet>`,
		"xl/metadata.xml":  `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><metadata/>`,
		"xl/calcChain.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><calcChain/>`,
	}
	for name, data := range parts {
		wc, err := w.Create(name)
		if err != nil {
			_ = w.Close()
			return err
		}
		if _, err := io.Copy(wc, bytes.NewBufferString(data)); err != nil {
			_ = w.Close()
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return nil
}

func readZipPart(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
