package sheetgrid

import (
	"path/filepath"
	"testing"
)

// TestRoundTripXLSX writes a grid, parses it back, and checks fidelity — the
// core of the render + edit→regenerate loop.
func TestRoundTripXLSX(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.xlsx")
	in := &Grid{
		Sheet:   "Companies",
		Columns: []string{"Rank", "Company", "Market Cap"},
		Rows: [][]string{
			{"1", "NVIDIA", "$4.9T"},
			{"2", "Apple", "$4.3T"},
		},
	}
	if err := Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Sheet != "Companies" {
		t.Errorf("sheet = %q, want Companies", got.Sheet)
	}
	if len(got.Columns) != 3 || got.Columns[1] != "Company" {
		t.Errorf("columns = %v", got.Columns)
	}
	if len(got.Rows) != 2 || got.Rows[0][1] != "NVIDIA" || got.Rows[1][2] != "$4.3T" {
		t.Errorf("rows = %v", got.Rows)
	}
	if got.TotalRows != 2 {
		t.Errorf("TotalRows = %d, want 2", got.TotalRows)
	}
}

// TestRoundTripCSV covers the csv path.
func TestRoundTripCSV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	in := &Grid{
		Columns: []string{"a", "b"},
		Rows:    [][]string{{"1", "2"}, {"3", "4"}},
	}
	if err := Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Columns) != 2 || len(got.Rows) != 2 || got.Rows[1][0] != "3" {
		t.Errorf("csv round-trip mismatch: %+v", got)
	}
}

// TestTruncation verifies row bounding flags truncation and keeps the true count.
func TestTruncation(t *testing.T) {
	raw := make([][]string, 0, MaxRows+11)
	raw = append(raw, []string{"col"})
	for i := 0; i < MaxRows+10; i++ {
		raw = append(raw, []string{"v"})
	}
	g := gridFromRows("", raw)
	if !g.Truncated {
		t.Error("expected Truncated=true")
	}
	if len(g.Rows) != MaxRows {
		t.Errorf("rows = %d, want %d (clipped)", len(g.Rows), MaxRows)
	}
	if g.TotalRows != MaxRows+10 {
		t.Errorf("TotalRows = %d, want %d (true count)", g.TotalRows, MaxRows+10)
	}
}

// TestRaggedRowsNormalized verifies short rows pad to header width.
func TestRaggedRowsNormalized(t *testing.T) {
	g := gridFromRows("", [][]string{
		{"a", "b", "c"},
		{"1"}, // ragged: only 1 cell
	})
	if len(g.Rows[0]) != 3 {
		t.Errorf("row width = %d, want 3 (padded to header)", len(g.Rows[0]))
	}
	if g.Rows[0][0] != "1" || g.Rows[0][2] != "" {
		t.Errorf("ragged row not normalized: %v", g.Rows[0])
	}
}

// TestUnsupportedType rejects non-spreadsheet extensions.
func TestUnsupportedType(t *testing.T) {
	if _, err := Parse("foo.pdf"); err == nil {
		t.Error("expected error for .pdf")
	}
}
