package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestMCPSheetWriter_BatchWrite_SendsCorrectPayload(t *testing.T) {
	var received map[string]any
	var receivedAuth string
	var receivedServiceToken string
	var receivedActorUser string
	var receivedActorOrg string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Errorf("path: want /mcp, got %s", r.URL.Path)
		}
		receivedAuth = r.Header.Get("Authorization")
		receivedServiceToken = r.Header.Get("X-Service-Token")
		receivedActorUser = r.Header.Get("X-Actor-User-ID")
		receivedActorOrg = r.Header.Get("X-Actor-Org-ID")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"text":"{}"}],"isError":false}}`))
	}))
	defer srv.Close()

	wr := NewMCPSheetWriter(srv.URL, "svc-token", "org-slug")
	err := wr.BatchWrite(context.Background(), "user-1", []CellWrite{
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 0, ColIdx: 0, Value: "Acme"},
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 0, ColIdx: 1, Value: "Jane"},
		{SpreadsheetID: "ss-1", SheetTab: "Sheet1", RowIdx: 1, ColIdx: 26, Value: "v"}, // tests AA column
	})
	if err != nil {
		t.Fatal(err)
	}

	if receivedServiceToken != "svc-token" {
		t.Errorf("X-Service-Token: want 'svc-token', got %q", receivedServiceToken)
	}
	if receivedAuth != "" {
		t.Errorf("Authorization header must NOT be set (sheets-mcp uses X-Service-Token), got %q", receivedAuth)
	}
	if receivedActorUser != "user-1" {
		t.Errorf("X-Actor-User-ID: want user-1, got %q", receivedActorUser)
	}
	if receivedActorOrg != "org-slug" {
		t.Errorf("X-Actor-Org-ID: want org-slug, got %q", receivedActorOrg)
	}

	method, _ := received["method"].(string)
	if method != "tools/call" {
		t.Errorf("method: want tools/call, got %s", method)
	}
	params, _ := received["params"].(map[string]any)
	if name, _ := params["name"].(string); name != "sheets_batch_update" {
		t.Errorf("tool name: want sheets_batch_update, got %s", name)
	}
	args, _ := params["arguments"].(map[string]any)
	if args["spreadsheet_id"] != "ss-1" {
		t.Errorf("spreadsheet_id: want ss-1, got %v", args["spreadsheet_id"])
	}
	updates, _ := args["updates"].([]any)
	if len(updates) != 3 {
		t.Fatalf("updates count: want 3, got %d", len(updates))
	}
	first, _ := updates[0].(map[string]any)
	if first["range"] != "Sheet1!A2" { // row 0 → A2 (header offset)
		t.Errorf("first range: want Sheet1!A2, got %v", first["range"])
	}
	third, _ := updates[2].(map[string]any)
	if third["range"] != "Sheet1!AA3" { // col 26 → AA, row 1 → row 3
		t.Errorf("third range: want Sheet1!AA3, got %v", third["range"])
	}
}

func TestMCPSheetWriter_NoOpOnEmpty(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "tok", "org")
	if err := wr.BatchWrite(context.Background(), "u", nil); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Errorf("HTTP should not be called on empty batch")
	}
}

func TestMCPSheetWriter_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream dead`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "tok", "org")
	err := wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("expected wrapped 502, got %v", err)
	}
}

func TestMCPSheetWriter_ToolErrorEnvelope(t *testing.T) {
	// MCP tool returned 200 but with isError + error text in content
	// (e.g. Google Sheets auth expired).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"text":"auth_failed: token revoked"}]}}`))
	}))
	defer srv.Close()
	wr := NewMCPSheetWriter(srv.URL, "tok", "org")
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
	wr := NewMCPSheetWriter(srv.URL, "tok", "org")
	_ = wr.BatchWrite(context.Background(), "u", []CellWrite{
		{SpreadsheetID: "ss", SheetTab: "", RowIdx: 0, ColIdx: 0, Value: "x"},
	})
	params, _ := received["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	updates, _ := args["updates"].([]any)
	first, _ := updates[0].(map[string]any)
	if first["range"] != "Sheet1!A2" {
		t.Errorf("default tab: want Sheet1!A2 range, got %v", first["range"])
	}
}
