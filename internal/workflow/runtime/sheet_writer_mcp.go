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
// through composio-mcp's `GOOGLESHEETS_VALUES_BATCH_UPDATE` synthetic
// tool. The sidecar routes this name through Composio's `tools.
// proxyExecute` to Google's NATIVE `spreadsheets.values.batchUpdate`
// API endpoint, with Composio injecting the user's OAuth credentials.
//
// Why proxy-execute (not a curated Composio action): Composio's
// curated `GOOGLESHEETS_BATCH_UPDATE` is a wrapper with its own opinion
// about argument shape and doesn't accept Google's native multi-range
// `data` array. Sending Google-native format gets rejected with
// "WRONG FORMAT: this tool requires a different schema". The
// `proxyExecute` path is exactly the escape valve Composio documents
// for "endpoint not covered by a predefined tool / request shape a
// predefined tool cannot express" — it's the upstream-recommended
// pattern, not a workaround.
//
// Why composio at all (not direct Google API): users already authorize
// Google through Composio's verified OAuth app for Gmail/Drive/etc.
// Composio-managed tokens are masked so we can't extract them, but
// proxyExecute lets us call Google directly with Composio injecting
// auth — keeping a single OAuth surface while gaining native API
// access.
//
// Batching: ONE proxyExecute call writes the entire wave (any number
// of cells across any number of distinct ranges) as ONE Google Sheets
// API request. Previous designs fanned out one call per cell and hit
// Google's "60 Write requests per minute per user" quota on any wave
// >60 cells; native batchUpdate sidesteps the quota entirely (1 wave
// = 1 quota unit regardless of cell count).
//
// Auth: composio-mcp listens on a private docker network and reads
// the acting user from `X-Proxy-User`. No service token (it's
// internal). The header value MUST be the goclaw-internal user UUID;
// composio-mcp resolves it to a Composio connectedAccountId and
// supplies that to proxyExecute.
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

// BatchWrite implements SheetWriter. Packs the whole wave's writes
// into ONE composio-mcp tools/call to GOOGLESHEETS_VALUES_BATCH_UPDATE
// (the sidecar's synthetic proxy tool) regardless of how many cells
// or how many distinct ranges they touch. Returns an error only on
// transport / auth failure of that single call — per-cell retries are
// the orchestrator's job at the next wave boundary.
func (w *MCPSheetWriter) BatchWrite(ctx context.Context, userID string, writes []CellWrite) error {
	if len(writes) == 0 {
		return nil
	}

	// All writes in a single BatchWrite target the same spreadsheet.
	// Each CellWrite becomes one entry in the batch `data` array
	// targeting a single-cell A1 range; Google's native batchUpdate
	// handles arbitrary mixed ranges in one round-trip. Field names
	// match Google's API verbatim (camelCase) — composio-mcp forwards
	// the body to `/v4/spreadsheets/{id}/values:batchUpdate` without
	// renaming, so the closer we are to Google's schema, the fewer
	// translation seams.
	spreadsheetID := writes[0].SpreadsheetID
	data := make([]map[string]any, 0, len(writes))
	for _, c := range writes {
		tab := c.SheetTab
		if tab == "" {
			tab = "Sheet1"
		}
		a1 := fmt.Sprintf("%s!%s%d", tab, colLetter(c.ColIdx), c.RowIdx+2)
		data = append(data, map[string]any{
			"range":  a1,
			"values": [][]any{{c.Value}},
		})
	}

	args := map[string]any{
		"spreadsheet_id":   spreadsheetID,
		"valueInputOption": "USER_ENTERED",
		"data":             data,
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "GOOGLESHEETS_VALUES_BATCH_UPDATE",
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
		return fmt.Errorf("composio GOOGLESHEETS_VALUES_BATCH_UPDATE %s: %s", resp.Status, truncate(buf.String(), 300))
	}

	// Composio-mcp wraps action results in MCP's content envelope. A
	// soft failure (e.g. auth_expired, quota) lands as result.isError=
	// true with a human-readable text payload; surface that as an
	// error so the orchestrator can retry / mark the run failed.
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
