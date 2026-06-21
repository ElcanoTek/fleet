package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

type XLSXParams struct {
	Action     string   `json:"action" description:"Action to perform: inspect, rename_sheet, set_cell."`
	Path       string   `json:"path" description:"Path to the .xlsx workbook."`
	OutputPath string   `json:"output_path,omitempty" description:"Optional output path. If omitted, edits are made in place."`
	SheetName  string   `json:"sheet_name,omitempty" description:"Worksheet name for set_cell, or current sheet name for rename_sheet."`
	NewName    string   `json:"new_name,omitempty" description:"New worksheet name for rename_sheet."`
	Cell       string   `json:"cell,omitempty" description:"Cell coordinate for set_cell, such as C1."`
	Value      string   `json:"value,omitempty" description:"Plain text value for set_cell."`
	Values     []string `json:"values,omitempty" description:"Reserved for future row operations."`
}

func NewXLSXTool() fantasy.AgentTool {
	description := `Safely inspects and performs targeted edits on .xlsx workbooks without rebuilding them through pandas/openpyxl. Use this for formula-heavy Excel files before uploading to Fast.io. Actions: inspect validates package/workbook XML, lists sheets, and reports formula cache coverage; rename_sheet updates the workbook sheet name in-place; set_cell changes one cell's text value by editing only the target worksheet XML. This preserves formulas, cached formula values, styles, images, and unrelated workbook parts.`
	return fantasy.NewAgentTool("xlsx_workbook", description,
		func(ctx context.Context, params XLSXParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			out, err := runXLSXAction(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(out), nil
		})
}

func runXLSXAction(ctx context.Context, params XLSXParams) (string, error) {
	path, err := ValidatePathForRead(resolveWorkspacePath(ctx, params.Path))
	if err != nil {
		return "", err
	}
	wb, err := readXLSXPackage(path)
	if err != nil {
		return "", err
	}
	switch params.Action {
	case "inspect":
		return inspectXLSX(wb)
	case "rename_sheet":
		if params.SheetName == "" || params.NewName == "" {
			return "", fmt.Errorf("rename_sheet requires sheet_name and new_name")
		}
		if err := renameXLSXSheet(wb, params.SheetName, params.NewName); err != nil {
			return "", err
		}
	case "set_cell":
		if params.SheetName == "" || params.Cell == "" {
			return "", fmt.Errorf("set_cell requires sheet_name and cell")
		}
		if err := setXLSXCell(wb, params.SheetName, params.Cell, params.Value); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported xlsx action %q", params.Action)
	}
	outPath := path
	if params.OutputPath != "" {
		outPath, err = ValidatePath(resolveWorkspacePath(ctx, params.OutputPath))
		if err != nil {
			return "", err
		}
	}
	if err := writeXLSXPackage(outPath, wb); err != nil {
		return "", err
	}
	result := map[string]any{"status": "success", "path": outPath, "action": params.Action}
	b, _ := json.MarshalIndent(result, "", "  ")
	return string(b), nil
}

type xlsxPackage struct {
	order []string
	files map[string][]byte
}

func readXLSXPackage(path string) (*xlsxPackage, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := &xlsxPackage{files: map[string][]byte{}}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		name := filepath.ToSlash(f.Name)
		out.order = append(out.order, name)
		out.files[name] = data
	}
	return out, nil
}

func writeXLSXPackage(path string, pkg *xlsxPackage) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "xlsx-tool-*.xlsx")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	w := zip.NewWriter(tmp)
	seen := map[string]bool{}
	for _, name := range pkg.order {
		data, ok := pkg.files[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		fw, err := w.Create(name)
		if err != nil {
			_ = w.Close()
			_ = tmp.Close()
			return err
		}
		if _, err := fw.Write(data); err != nil {
			_ = w.Close()
			_ = tmp.Close()
			return err
		}
	}
	if err := w.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func inspectXLSX(pkg *xlsxPackage) (string, error) {
	workbook := pkg.files["xl/workbook.xml"]
	if len(workbook) == 0 {
		return "", fmt.Errorf("missing xl/workbook.xml")
	}
	xmlValid := xml.Unmarshal(workbook, new(any)) == nil
	sheets := xlsxSheetMap(string(workbook))
	formulaCells, cachedFormulaCells := 0, 0
	cellWithFormulaRe := regexp.MustCompile(`(?s)<c\b[^>]*>.*?<f\b.*?</f>.*?</c>`)
	for name, data := range pkg.files {
		if !strings.HasPrefix(name, "xl/worksheets/") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		for _, m := range cellWithFormulaRe.FindAll(data, -1) {
			formulaCells++
			if bytes.Contains(m, []byte("<v>")) {
				cachedFormulaCells++
			}
		}
	}
	res := map[string]any{
		"status":               "success",
		"workbook_xml_valid":   xmlValid,
		"sheet_names":          sheetNames(sheets),
		"formula_cells":        formulaCells,
		"cached_formula_cells": cachedFormulaCells,
		"has_metadata_part":    pkg.files["xl/metadata.xml"] != nil,
		"has_calc_chain":       pkg.files["xl/calcChain.xml"] != nil,
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	return string(b), nil
}

type xlsxSheet struct{ Name, RelID string }

func xlsxSheetMap(workbook string) []xlsxSheet {
	re := regexp.MustCompile(`<sheet\b[^>]*\bname="([^"]+)"[^>]*\br:id="([^"]+)"[^>]*/>`)
	matches := re.FindAllStringSubmatch(workbook, -1)
	out := make([]xlsxSheet, 0, len(matches))
	for _, m := range matches {
		out = append(out, xlsxSheet{Name: unescapeXML(m[1]), RelID: m[2]})
	}
	return out
}

func sheetNames(sheets []xlsxSheet) []string {
	out := make([]string, 0, len(sheets))
	for _, s := range sheets {
		out = append(out, s.Name)
	}
	return out
}

func renameXLSXSheet(pkg *xlsxPackage, oldName, newName string) error {
	wb := string(pkg.files["xl/workbook.xml"])
	oldAttr := `name="` + regexp.QuoteMeta(escapeXMLAttr(oldName)) + `"`
	re := regexp.MustCompile(oldAttr)
	if !re.MatchString(wb) {
		return fmt.Errorf("sheet %q not found", oldName)
	}
	wb = re.ReplaceAllString(wb, `name="`+escapeXMLAttr(newName)+`"`)
	wb = strings.ReplaceAll(wb, "'"+oldName+"'!", "'"+newName+"'!")
	wb = strings.ReplaceAll(wb, ">"+oldName+"!", ">"+newName+"!")
	pkg.files["xl/workbook.xml"] = []byte(wb)
	if app, ok := pkg.files["docProps/app.xml"]; ok {
		s := strings.ReplaceAll(string(app), ">"+escapeXMLText(oldName)+"<", ">"+escapeXMLText(newName)+"<")
		pkg.files["docProps/app.xml"] = []byte(s)
	}
	return nil
}

func setXLSXCell(pkg *xlsxPackage, sheetName, cell, value string) error {
	sheetPath, err := xlsxWorksheetPath(pkg, sheetName)
	if err != nil {
		return err
	}
	cell = strings.ToUpper(strings.TrimSpace(cell))
	if !regexp.MustCompile(`^[A-Z]+[1-9][0-9]*$`).MatchString(cell) {
		return fmt.Errorf("invalid cell coordinate %q", cell)
	}
	rowNum := cellRow(cell)
	xmlText := string(pkg.files[sheetPath])
	cellXML := `<c r="` + cell + `" t="inlineStr"><is><t>` + escapeXMLText(value) + `</t></is></c>`
	cellRe := regexp.MustCompile(`(?s)<c\b[^>]*\br="` + regexp.QuoteMeta(cell) + `"[^>]*>.*?</c>`)
	if cellRe.MatchString(xmlText) {
		xmlText = cellRe.ReplaceAllString(xmlText, cellXML)
		pkg.files[sheetPath] = []byte(xmlText)
		return nil
	}
	rowRe := regexp.MustCompile(`(?s)<row\b[^>]*\br="` + strconv.Itoa(rowNum) + `"[^>]*>.*?</row>`)
	if rowRe.MatchString(xmlText) {
		xmlText = rowRe.ReplaceAllStringFunc(xmlText, func(row string) string {
			return strings.Replace(row, "</row>", cellXML+"</row>", 1)
		})
		pkg.files[sheetPath] = []byte(xmlText)
		return nil
	}
	rowXML := `<row r="` + strconv.Itoa(rowNum) + `">` + cellXML + `</row>`
	if !strings.Contains(xmlText, "</sheetData>") {
		return fmt.Errorf("worksheet %q has no sheetData", sheetName)
	}
	xmlText = strings.Replace(xmlText, "</sheetData>", rowXML+"</sheetData>", 1)
	pkg.files[sheetPath] = []byte(xmlText)
	return nil
}

func xlsxWorksheetPath(pkg *xlsxPackage, sheetName string) (string, error) {
	sheets := xlsxSheetMap(string(pkg.files["xl/workbook.xml"]))
	relID := ""
	for _, s := range sheets {
		if s.Name == sheetName {
			relID = s.RelID
			break
		}
	}
	if relID == "" {
		return "", fmt.Errorf("sheet %q not found", sheetName)
	}
	rels := string(pkg.files["xl/_rels/workbook.xml.rels"])
	re := regexp.MustCompile(`<Relationship\b[^>]*\bId="` + regexp.QuoteMeta(relID) + `"[^>]*\bTarget="([^"]+)"[^>]*/>`)
	m := re.FindStringSubmatch(rels)
	if len(m) < 2 {
		return "", fmt.Errorf("relationship %q for sheet %q not found", relID, sheetName)
	}
	target := filepath.ToSlash(m[1])
	if strings.HasPrefix(target, "/") {
		target = strings.TrimPrefix(target, "/")
	} else {
		target = "xl/" + target
	}
	if pkg.files[target] == nil {
		return "", fmt.Errorf("worksheet part %q not found", target)
	}
	return target, nil
}

func cellRow(cell string) int {
	i := 0
	for i < len(cell) && (cell[i] < '0' || cell[i] > '9') {
		i++
	}
	n, _ := strconv.Atoi(cell[i:])
	return n
}

func escapeXMLAttr(s string) string { return html.EscapeString(s) }
func escapeXMLText(s string) string { return html.EscapeString(s) }
func unescapeXML(s string) string {
	out := strings.ReplaceAll(s, "&quot;", `"`)
	out = strings.ReplaceAll(out, "&apos;", "'")
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&amp;", "&")
	return out
}
