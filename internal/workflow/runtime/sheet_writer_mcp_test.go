package runtime

import (
	"context"
	"encoding/json"
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

// TestMCPSheetWriter_PerCell_ComposioRouting asserts the writer issues
// one GOOGLESHEETS_VALUES_UPDATE composio-mcp call per CellWrite with
// X-Proxy-User identity and the right A1 ranges. Composio's allowlist
// doesn't include a batch update, so per-cell fan-out is intentional —
// orchestrator concurrency already absorbs the overhead.
func TestMCPSheetWriter_PerCell_ComposioRouting(t *testing.T) {
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
	err := wr.BatchWrite(context.Background(), "user-1", []CellWrite{
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 0, ColIdx: 0, Value: "Acme"},
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 0, ColIdx: 1, Value: "Jane"},
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 1, ColIdx: 26, Value: "v"}, // AA column
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(calls) != 3 {
		t.Fatalf("want 3 composio calls, got %d", len(calls))
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

	// Range mapping: row 0 → row 2 (header offset), col 26 → AA.
	args0, _ := calls[0].body["params"].(map[string]any)["arguments"].(map[string]any)
	if args0["range"] != "Sheet1!A2" {
		t.Errorf("cell 0 range: want Sheet1!A2, got %v", args0["range"])
	}
	args2, _ := calls[2].body["params"].(map[string]any)["arguments"].(map[string]any)
	if args2["range"] != "Sheet1!AA3" {
		t.Errorf("cell 2 range: want Sheet1!AA3, got %v", args2["range"])
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
	if args["range"] != "Sheet1!A2" {
		t.Errorf("default tab: want Sheet1!A2 range, got %v", args["range"])
	}
}
