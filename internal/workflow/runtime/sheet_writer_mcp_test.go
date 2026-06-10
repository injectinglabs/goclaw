package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestColLetter(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "A"}, {1, "B"}, {25, "Z"}, {26, "AA"}, {27, "AB"},
		{51, "AZ"}, {52, "BA"}, {701, "ZZ"}, {702, "AAA"},
		{-1, "A"}, // defensive
	}
	for _, c := range cases {
		got := colLetter(c.in)
		if got != c.want {
			t.Errorf("colLetter(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMCPSheetWriter_PerColumnPacking asserts that contiguous-row
// cells in the same column collapse into ONE composio VALUES_UPDATE
// call with a multi-row range — the mechanism that keeps a typical
// 100+-cell wave under Google's 60/min write quota.
func TestMCPSheetWriter_PerColumnPacking(t *testing.T) {
	type recv struct {
		path        string
		proxyUser   string
		authPresent bool
		svcPresent  bool
		body        map[string]any
	}
	var calls []recv

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var bodyMap map[string]any
		_ = json.Unmarshal(body, &bodyMap)
		calls = append(calls, recv{
			path:        r.URL.Path,
			proxyUser:   r.Header.Get("X-Proxy-User"),
			authPresent: r.Header.Get("Authorization") != "",
			svcPresent:  r.Header.Get("X-Service-Token") != "",
			body:        bodyMap,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"text":"{}"}],"isError":false}}`))
	}))
	defer srv.Close()

	wr := NewMCPSheetWriter(srv.URL, "ignored-legacy-token", "org-slug")
	// Two contiguous rows in col 0, then a single cell in col 26 (AA).
	// Expect 2 calls: one for the col-0 run (range A2:A3), one for the
	// col-AA single cell (range AA3:AA3).
	err := wr.BatchWrite(context.Background(), "user-1", []CellWrite{
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 0, ColIdx: 0, Value: "Acme"},
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 1, ColIdx: 0, Value: "Beta"},
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 1, ColIdx: 26, Value: "v"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(calls) != 2 {
		t.Fatalf("want 2 composio calls (col-0 run + col-AA single), got %d", len(calls))
	}
	for i, c := range calls {
		if c.path != "/mcp" {
			t.Errorf("call %d path: want /mcp, got %s", i, c.path)
		}
		if c.proxyUser != "user-1" {
			t.Errorf("call %d X-Proxy-User: want user-1, got %q", i, c.proxyUser)
		}
		if c.authPresent {
			t.Errorf("call %d must NOT send Authorization (composio is unauth on internal net)", i)
		}
		if c.svcPresent {
			t.Errorf("call %d must NOT send X-Service-Token (that was the retired sheets-mcp path)", i)
		}
		params, _ := c.body["params"].(map[string]any)
		if name, _ := params["name"].(string); name != "GOOGLESHEETS_VALUES_UPDATE" {
			t.Errorf("call %d tool name: want GOOGLESHEETS_VALUES_UPDATE, got %s", i, name)
		}
		args, _ := params["arguments"].(map[string]any)
		if args["spreadsheet_id"] != "ss-1" {
			t.Errorf("call %d spreadsheet_id: got %v", i, args["spreadsheet_id"])
		}
	}

	// First call: col 0 run packs 2 cells into A2:A3.
	args0, _ := calls[0].body["params"].(map[string]any)["arguments"].(map[string]any)
	if args0["range"] != "Sheet1!A2:A3" {
		t.Errorf("call 0 range: want Sheet1!A2:A3, got %v", args0["range"])
	}
	values0, ok := args0["values"].([]any)
	if !ok || len(values0) != 2 {
		t.Errorf("call 0 values: want 2 rows, got %v", args0["values"])
	}
	// Second call: AA single cell at row 1 → AA3:AA3.
	args1, _ := calls[1].body["params"].(map[string]any)["arguments"].(map[string]any)
	if args1["range"] != "Sheet1!AA3:AA3" {
		t.Errorf("call 1 range: want Sheet1!AA3:AA3, got %v", args1["range"])
	}
}

// TestGroupContiguousRuns verifies the run-packing logic directly so
// regressions on the grouping invariant surface without needing the
// HTTP server.
func TestGroupContiguousRuns(t *testing.T) {
	runs := groupContiguousRuns([]CellWrite{
		// col 0 rows 0,1,2 (run of 3)
		{SpreadsheetID: "ss", SheetTab: "S1", RowIdx: 0, ColIdx: 0, Value: "a"},
		{SpreadsheetID: "ss", SheetTab: "S1", RowIdx: 1, ColIdx: 0, Value: "b"},
		{SpreadsheetID: "ss", SheetTab: "S1", RowIdx: 2, ColIdx: 0, Value: "c"},
		// col 0 row 5 (split — gap at rows 3,4)
		{SpreadsheetID: "ss", SheetTab: "S1", RowIdx: 5, ColIdx: 0, Value: "d"},
		// col 1 rows 0,1 (different column → separate run)
		{SpreadsheetID: "ss", SheetTab: "S1", RowIdx: 0, ColIdx: 1, Value: "x"},
		{SpreadsheetID: "ss", SheetTab: "S1", RowIdx: 1, ColIdx: 1, Value: "y"},
	})
	if len(runs) != 3 {
		t.Fatalf("want 3 runs (col0 rows 0-2, col0 row 5, col1 rows 0-1), got %d", len(runs))
	}
	if runs[0].ColIdx != 0 || runs[0].StartRow != 0 || len(runs[0].Values) != 3 {
		t.Errorf("run 0: want col=0 start=0 len=3, got col=%d start=%d len=%d", runs[0].ColIdx, runs[0].StartRow, len(runs[0].Values))
	}
	if runs[1].ColIdx != 0 || runs[1].StartRow != 5 || len(runs[1].Values) != 1 {
		t.Errorf("run 1: want col=0 start=5 len=1, got col=%d start=%d len=%d", runs[1].ColIdx, runs[1].StartRow, len(runs[1].Values))
	}
	if runs[2].ColIdx != 1 || runs[2].StartRow != 0 || len(runs[2].Values) != 2 {
		t.Errorf("run 2: want col=1 start=0 len=2, got col=%d start=%d len=%d", runs[2].ColIdx, runs[2].StartRow, len(runs[2].Values))
	}
}

func TestMCPSheetWriter_NoOpOnEmpty(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "", "org")
	if err := wr.BatchWrite(context.Background(), "u", nil); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 0 {
		t.Errorf("HTTP must not be called on empty batch")
	}
}

func TestMCPSheetWriter_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream dead`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "", "org")
	err := wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("expected wrapped 502, got %v", err)
	}
}

func TestMCPSheetWriter_ToolErrorEnvelope(t *testing.T) {
	// Composio returns 200 + result.isError=true for soft failures
	// (e.g. composio user has no Google connection). Treat as error
	// so the orchestrator can retry / mark the cell failed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"text":"auth_failed: token revoked"}]}}`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "", "org")
	err := wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "auth_failed") {
		t.Errorf("expected tool-error to propagate, got %v", err)
	}
}

// TestMCPSheetWriter_RetryOnRateLimit verifies that a transient
// RESOURCE_EXHAUSTED envelope is retried with backoff and the wave
// eventually succeeds. Without this, a brief 60/min spike (e.g. a
// burst run kicked off right after another) would fail the whole
// wave instead of draining through.
func TestMCPSheetWriter_RetryOnRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			// First two calls — Google quota exhaustion soft error.
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"text":"RESOURCE_EXHAUSTED: Quota exceeded for quota metric 'Write requests per minute per user'"}]}}`))
			return
		}
		// Third call — success.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"text":"{}"}],"isError":false}}`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "", "org")
	err := wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	if err != nil {
		t.Fatalf("BatchWrite: want success after retries, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("want 3 attempts (2 rate-limited + 1 success), got %d", got)
	}
}

// TestMCPSheetWriter_NonRetryableAuthErrorFailsFast verifies the
// retry loop does NOT eat the backoff budget on errors that won't
// recover — auth failures, validation, etc.
func TestMCPSheetWriter_NonRetryableAuthErrorFailsFast(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"text":"auth_failed: token revoked"}]}}`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "", "org")
	err := wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	if err == nil {
		t.Fatal("want error on auth failure, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("auth failure must NOT retry — want 1 attempt, got %d", got)
	}
}

// TestIsRateLimitError covers the substring patterns we treat as
// rate-limit signals. Keep this in sync with isRateLimitError's
// allow-list — false positives here would cause auth errors to spin
// in the retry loop until exhaustion.
func TestIsRateLimitError(t *testing.T) {
	rate := []string{
		"composio GOOGLESHEETS_VALUES_UPDATE 429: too many",
		"composio tool error: RESOURCE_EXHAUSTED: Quota exceeded",
		"Quota exceeded for quota metric 'Write requests per minute per user'",
		"rate limit hit",
		"Too Many Requests",
	}
	for _, m := range rate {
		if !isRateLimitError(fmt.Errorf("%s", m)) {
			t.Errorf("expected rate-limit: %q", m)
		}
	}
	notRate := []string{
		"composio tool error: auth_failed: token revoked",
		"Invalid range: Sheet1!XX99",
		"network: connection refused",
		"",
	}
	for _, m := range notRate {
		if isRateLimitError(fmt.Errorf("%s", m)) {
			t.Errorf("expected NOT rate-limit: %q", m)
		}
	}
}

func TestMCPSheetWriter_DefaultSheetTab(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "", "org")
	_ = wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", SheetTab: "", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	params, _ := received["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if args["range"] != "Sheet1!A2:A2" {
		t.Errorf("default tab: want Sheet1!A2:A2 range, got %v", args["range"])
	}
}
