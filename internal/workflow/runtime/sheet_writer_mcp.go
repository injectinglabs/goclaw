package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MCPSheetWriter writes cell values back to the user's Google Sheet
// via sheets-mcp's `sheets_batch_update` MCP tool. The MCP protocol
// is plain HTTP JSON-RPC at /mcp on the sidecar — we don't need the
// mcp-go client library, a tiny shim is enough.
//
// Headers passed verbatim from the orchestrator caller so per-cell
// billing attributes correctly to the workflow owner's org:
//
//	X-Actor-User-ID, X-Actor-Org-ID, Authorization (service token).
//
// Retries are NOT done here — the orchestrator already retries failed
// cells per CellExecutor, and writer failure fails the run (the user's
// sheet diverging from DB state is a correctness issue, not transient).
type MCPSheetWriter struct {
	mcpURL       string
	serviceToken string
	orgID        string
	httpClient   *http.Client
}

// NewMCPSheetWriter constructs a writer for the given sheets-mcp URL
// (e.g. "http://sheets-mcp.injecting.ai" or local docker
// "http://sheets-mcp:9300"). `serviceToken` is the gateway bearer
// shared between goclaw and the sheets-mcp sidecar. `orgID` is the
// tenant's external org id (slug or UUID) used in X-Actor-Org-ID.
func NewMCPSheetWriter(mcpURL, serviceToken, orgID string) *MCPSheetWriter {
	return &MCPSheetWriter{
		mcpURL:       strings.TrimRight(mcpURL, "/"),
		serviceToken: serviceToken,
		orgID:        orgID,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// BatchWrite implements SheetWriter. Groups writes by spreadsheet
// (orchestrator only fans out one workflow per run, so this collapses
// to a single batch) and calls sheets_batch_update.
func (w *MCPSheetWriter) BatchWrite(ctx context.Context, userID string, writes []CellWrite) error {
	if len(writes) == 0 {
		return nil
	}
	spreadsheetID := writes[0].SpreadsheetID
	tab := writes[0].SheetTab
	if tab == "" {
		tab = "Sheet1"
	}

	updates := make([]map[string]any, 0, len(writes))
	for _, c := range writes {
		// rowIdx is 0-based inside target_range. The sheet header is
		// at row 1, so the first data row sits at row 2 → rowIdx 0
		// maps to A1 row 2. colIdx 0 → column A.
		a1 := fmt.Sprintf("%s!%s%d", tab, colLetter(c.ColIdx), c.RowIdx+2)
		updates = append(updates, map[string]any{
			"range":  a1,
			"values": [][]any{{c.Value}},
		})
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "sheets_batch_update",
			"arguments": map[string]any{
				"user_id":            userID,
				"spreadsheet_id":     spreadsheetID,
				"updates":            updates,
				"value_input_option": "USER_ENTERED",
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal mcp request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.mcpURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build mcp request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if w.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.serviceToken)
	}
	req.Header.Set("X-Actor-User-ID", userID)
	if w.orgID != "" {
		req.Header.Set("X-Actor-Org-ID", w.orgID)
	}

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("mcp sheets_batch_update %s: %s", resp.Status, truncate(buf.String(), 300))
	}

	// We don't parse the response — success status is enough; the
	// orchestrator already has per-cell status in its own DB and the
	// MCP tool's own error envelope surfaces auth/quota failures via
	// non-2xx (it returns 200 with body.error for soft failures, so
	// also inspect that).
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&rpc); err == nil {
		if rpc.Error != nil {
			return fmt.Errorf("mcp rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
		}
		if rpc.Result.IsError && len(rpc.Result.Content) > 0 {
			return fmt.Errorf("mcp tool error: %s", truncate(rpc.Result.Content[0].Text, 300))
		}
	}
	return nil
}

// colLetter converts a 0-based column index to A1 column letters.
// 0→A, 25→Z, 26→AA, 701→ZZ, 702→AAA.
func colLetter(idx int) string {
	if idx < 0 {
		return "A"
	}
	var b []byte
	n := idx + 1
	for n > 0 {
		n--
		b = append([]byte{byte('A' + n%26)}, b...)
		n /= 26
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
