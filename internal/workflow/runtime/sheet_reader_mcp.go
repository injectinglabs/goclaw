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

// MCPSheetReader fetches the current values of a Google Sheet range
// via composio-mcp's GOOGLESHEETS_VALUES_GET tool. It's the read-side
// counterpart of MCPSheetWriter and serves the SPA's
// `workflow.peekSheet` WS RPC.
//
// We read straight from the sheet (the source of truth that the
// orchestrator writes to) rather than from goclaw's
// sheet_workflow_cells table. The DB cache is a side-effect of the
// orchestrator's bookkeeping — it can drift (wrong tenant, expired
// run, foreign workflow). The sheet itself is what the user looks at.
type MCPSheetReader struct {
	composioURL string
	httpClient  *http.Client
}

// NewMCPSheetReader builds a reader pointing at the same composio-mcp
// sidecar the writer uses. composioURL is the base (no trailing slash).
func NewMCPSheetReader(composioURL string) *MCPSheetReader {
	return &MCPSheetReader{
		composioURL: strings.TrimRight(composioURL, "/"),
		httpClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

// ReadRange reads values from a Google Sheet range and returns them as
// a row-major 2-D string slice. Empty trailing cells in each row are
// preserved as empty strings so the SPA gets a rectangular grid.
//
// userID is the calling user — passed as X-Proxy-User so composio
// uses that user's OAuth token (NOT the goclaw service identity).
// This is what makes the call respect Google's own ACL: users only
// see sheets they themselves have access to, regardless of goclaw
// tenant state.
func (r *MCPSheetReader) ReadRange(
	ctx context.Context,
	userID, spreadsheetID, a1Range string,
) ([][]string, error) {
	if r == nil || r.composioURL == "" {
		return nil, fmt.Errorf("sheet reader not configured")
	}
	if userID == "" {
		return nil, fmt.Errorf("userID is required")
	}
	if spreadsheetID == "" {
		return nil, fmt.Errorf("spreadsheet_id is required")
	}
	if a1Range == "" {
		return nil, fmt.Errorf("range is required")
	}

	args := map[string]any{
		"spreadsheet_id": spreadsheetID,
		"range":          a1Range,
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "GOOGLESHEETS_VALUES_GET",
			"arguments": args,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal composio request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", r.composioURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build composio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Proxy-User", userID)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("composio call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("composio GOOGLESHEETS_VALUES_GET %s: %s", resp.Status, truncate(buf.String(), 300))
	}

	// MCP wraps tool output in `result.content[*].text`. Composio puts
	// a JSON document inside the first text item.
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return nil, fmt.Errorf("decode composio response: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("composio rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if rpc.Result.IsError && len(rpc.Result.Content) > 0 {
		return nil, fmt.Errorf("composio tool error: %s", truncate(rpc.Result.Content[0].Text, 300))
	}
	if len(rpc.Result.Content) == 0 {
		return nil, fmt.Errorf("composio returned no content")
	}

	// Inner JSON has shape: {"values": [[...], [...]], "range": "...", ...}
	var inner struct {
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &inner); err != nil {
		return nil, fmt.Errorf("decode composio values payload: %w", err)
	}

	// Normalise: every value is stringified; rows are padded to the
	// widest row so the SPA sees a rectangular grid. NOTE: an empty
	// sheet returns nil — caller treats that as "no data yet".
	if len(inner.Values) == 0 {
		return nil, nil
	}
	widest := 0
	for _, row := range inner.Values {
		if len(row) > widest {
			widest = len(row)
		}
	}
	out := make([][]string, len(inner.Values))
	for i, row := range inner.Values {
		out[i] = make([]string, widest)
		for j, cell := range row {
			out[i][j] = anyToString(cell)
		}
	}
	return out, nil
}

func anyToString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers come back as float64. Trim trailing .0 so years
		// like "2015" render cleanly instead of "2015.000000".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
