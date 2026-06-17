// Package sheetgrid parses spreadsheet files (.xlsx / .csv) into a compact,
// JSON-serializable grid the chat UI can render as an interactive table, and
// writes an edited grid back out to a file. It is the shared core behind the
// `sheet.preview` RPC (render) and the edit→regenerate path (the user edits the
// grid, the agent rewrites the deliverable).
package sheetgrid

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Bounds keep a preview cheap and the payload small — a chat-inline grid isn't
// a full spreadsheet viewer. Callers needing the whole file read it directly.
const (
	MaxRows = 500
	MaxCols = 60
)

// Grid is the wire shape sent to the client and accepted back on edit.
type Grid struct {
	Sheet     string     `json:"sheet"`             // worksheet name (xlsx) or "" (csv)
	Columns   []string   `json:"columns"`           // header row (first row)
	Rows      [][]string `json:"rows"`              // data rows (header excluded)
	Truncated bool       `json:"truncated"`         // true if MaxRows/MaxCols clipped the data
	TotalRows int        `json:"total_rows"`        // data-row count before truncation
}

// Parse reads a spreadsheet file into a Grid. The first row is treated as the
// header. Supports .xlsx (first/active sheet) and .csv by extension.
func Parse(path string) (*Grid, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return parseCSV(path)
	case ".xlsx", ".xlsm":
		return parseXLSX(path)
	default:
		return nil, fmt.Errorf("unsupported spreadsheet type %q (want .xlsx or .csv)", filepath.Ext(path))
	}
}

func parseXLSX(path string) (*Grid, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("open xlsx: %w", err)
	}
	defer f.Close() //nolint:errcheck

	sheet := f.GetSheetName(f.GetActiveSheetIndex())
	if sheet == "" {
		if names := f.GetSheetList(); len(names) > 0 {
			sheet = names[0]
		}
	}
	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return gridFromRows(sheet, rows), nil
}

func parseCSV(path string) (*Grid, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open csv: %w", err)
	}
	defer fh.Close() //nolint:errcheck

	r := csv.NewReader(fh)
	r.FieldsPerRecord = -1 // tolerate ragged rows
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read csv: %w", err)
	}
	return gridFromRows("", rows), nil
}

// gridFromRows turns a raw [][]string (header + data) into a bounded Grid,
// normalizing ragged rows to the header width.
func gridFromRows(sheet string, raw [][]string) *Grid {
	g := &Grid{Sheet: sheet, Columns: []string{}, Rows: [][]string{}}
	if len(raw) == 0 {
		return g
	}

	header := raw[0]
	if len(header) > MaxCols {
		header = header[:MaxCols]
		g.Truncated = true
	}
	width := len(header)
	g.Columns = append(g.Columns, header...)

	data := raw[1:]
	g.TotalRows = len(data)
	if len(data) > MaxRows {
		data = data[:MaxRows]
		g.Truncated = true
	}
	for _, r := range data {
		row := make([]string, width)
		for i := 0; i < width; i++ {
			if i < len(r) {
				row[i] = r[i]
			}
		}
		g.Rows = append(g.Rows, row)
	}
	return g
}

// Write serializes a (possibly edited) Grid back to a spreadsheet file, chosen
// by extension. Used by the edit→regenerate path so a user's inline edits
// produce a fresh downloadable file. Columns become row 1, then each data row.
func Write(path string, g *Grid) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return writeCSV(path, g)
	case ".xlsx", ".xlsm":
		return writeXLSX(path, g)
	default:
		return fmt.Errorf("unsupported spreadsheet type %q", filepath.Ext(path))
	}
}

func writeXLSX(path string, g *Grid) error {
	f := excelize.NewFile()
	defer f.Close() //nolint:errcheck
	sheet := g.Sheet
	if sheet == "" {
		sheet = "Sheet1"
	}
	idx, err := f.NewSheet(sheet)
	if err != nil {
		return fmt.Errorf("new sheet: %w", err)
	}
	f.SetActiveSheet(idx)
	// Remove the default "Sheet1" if we named ours something else.
	if sheet != "Sheet1" {
		_ = f.DeleteSheet("Sheet1")
	}
	if err := writeRow(f, sheet, 1, g.Columns); err != nil {
		return err
	}
	for i, r := range g.Rows {
		if err := writeRow(f, sheet, i+2, r); err != nil {
			return err
		}
	}
	return f.SaveAs(path)
}

func writeRow(f *excelize.File, sheet string, rowNum int, vals []string) error {
	for c, v := range vals {
		cell, err := excelize.CoordinatesToCellName(c+1, rowNum)
		if err != nil {
			return err
		}
		if err := f.SetCellStr(sheet, cell, v); err != nil {
			return err
		}
	}
	return nil
}

func writeCSV(path string, g *Grid) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close() //nolint:errcheck
	w := csv.NewWriter(fh)
	defer w.Flush()
	if err := w.Write(g.Columns); err != nil {
		return err
	}
	return w.WriteAll(g.Rows)
}
