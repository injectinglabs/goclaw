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

// MCPSheetWriter writes cell values back to the user's Google Sheet by
// driving composio-mcp's `GOOGLESHEETS_VALUES_UPDATE` action. Composio
// holds the user's already-established Google OAuth — the orchestrator
// piggybacks on it instead of running a parallel OAuth flow, which gave
// users a confusing second "Connect Google" prompt.
//
// Why composio (not sheets-mcp's batch_update): users already authorize
// Google through Composio's verified OAuth app for Gmail/Drive/etc.
// Maintaining a separate sheets-mcp OAuth doubled the consent step and
// kept two token stores in sync. Routing the writer through composio-
// mcp consolidates to one auth surface.
//
// Per-cell trade-off: Composio's allowlist exposes only single-range
// `VALUES_UPDATE` (no native batch). The orchestrator already has DAG
// concurrency + retry, so we fan out one composio call per CellWrite —
// at Sheet Workflows scale (≤500 cells per run) this is cheaper than
// adding a batchUpdate action to the composio surface.
//
// Auth: composio-mcp listens on a private docker network and reads the
// acting user from `X-Proxy-User`. No service token (it's internal).
// The header value MUST be the goclaw-internal user UUID; composio
// maps it to the user's connected Google account.
type MCPSheetWriter struct {
	// composioURL is the base URL of composio-mcp (e.g.
	// http://composio-mcp:9300). The writer appends /mcp.
	composioURL string
	// orgID is currently informational — kept on the struct so future
	// per-org attribution headers can be added without changing the
	// SheetWriter contract.
	orgID      string
	httpClient *http.Client
}

// NewMCPSheetWriter constructs a composio-backed writer. composioURL is
// the URL of the composio-mcp sidecar; orgID is the tenant's external
// org id (kept for forward compatibility — composio identifies via
// X-Proxy-User only today).
func NewMCPSheetWriter(composioURL, _unusedLegacyToken, orgID string) *MCPSheetWriter {
	// _unusedLegacyToken: kept in the signature to avoid touching all
	// call sites in this PR. The old sheets-mcp X-Service-Token is not
	// sent anywhere — composio-mcp runs unauthenticated on the docker
	// internal network and uses X-Proxy-User for identity.
	return &MCPSheetWriter{
		composioURL: strings.TrimRight(composioURL, "/"),
		orgID:       orgID,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// BatchWrite implements SheetWriter. Fans out one composio
// GOOGLESHEETS_VALUES_UPDATE call per CellWrite. Returns the FIRST
// error to fail-fast on auth/quota; subsequent already-issued calls
// continue to completion (their results are discarded — orchestrator
// will retry the failed cell on the next wave).
func (w *MCPSheetWriter) BatchWrite(ctx context.Context, userID string, writes []CellWrite) error {
	if len(writes) == 0 {
		return nil
	}
	for _, c := range writes {
		if err := w.writeCell(ctx, userID, c); err != nil {
			return err
		}
	}
	return nil
}

// writeCell issues a single GOOGLESHEETS_VALUES_UPDATE call through
// composio-mcp. The A1 range follows the header-offset convention used
// throughout Sheet Workflows: row 1 is the header, rowIdx 0 → row 2.
func (w *MCPSheetWriter) writeCell(ctx context.Context, userID string, c CellWrite) error {
	tab := c.SheetTab
	if tab == "" {
		tab = "Sheet1"
	}
	a1 := fmt.Sprintf("%s!%s%d", tab, colLetter(c.ColIdx), c.RowIdx+2)

	// Composio's GOOGLESHEETS_VALUES_UPDATE argument schema.
	// `values` must be 2-D (rows × cells). Single cell → [[value]].
	args := map[string]any{
		"spreadsheet_id":      c.SpreadsheetID,
		"range":               a1,
		"values":              [][]any{{c.Value}},
		"value_input_option":  "USER_ENTERED",
		"include_values_in_response": false,
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "GOOGLESHEETS_VALUES_UPDATE",
			"arguments": args,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal composio request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.composioURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build composio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Proxy-User", userID)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("composio call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("composio GOOGLESHEETS_VALUES_UPDATE %s: %s", resp.Status, truncate(buf.String(), 300))
	}

	// Composio-mcp wraps action results in MCP's content envelope. A
	// soft failure (e.g. auth_expired) lands as result.isError=true
	// with a human-readable text payload; surface that as an error so
	// the orchestrator can retry / mark the cell failed.
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
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err == nil {
		if rpc.Error != nil {
			return fmt.Errorf("composio rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
		}
		if rpc.Result.IsError && len(rpc.Result.Content) > 0 {
			return fmt.Errorf("composio tool error: %s", truncate(rpc.Result.Content[0].Text, 300))
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
