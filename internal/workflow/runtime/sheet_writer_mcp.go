package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"
)

// MCPSheetWriter writes cell values back to the user's Google Sheet
// through composio-mcp's `GOOGLESHEETS_VALUES_UPDATE` action. Composio
// holds the user's already-established Google OAuth — the orchestrator
// piggybacks on it instead of running a parallel OAuth flow, which
// gave users a confusing second "Connect Google" prompt.
//
// Why composio (not sheets-mcp's batch_update): users already
// authorize Google through Composio's verified OAuth app for Gmail/
// Drive/etc. Maintaining a separate sheets-mcp OAuth doubled the
// consent step and kept two token stores in sync. Routing the writer
// through composio-mcp consolidates to one auth surface.
//
// Batching strategy — per-column run packing:
//
// A naive one-cell-per-call fan-out hits Google's "60 Write requests
// per minute per user" quota on any wave with >60 cells (typical for
// 20-row × 6-col enrichments). The ideal solution would be Google's
// native `values.batchUpdate` (one API call for arbitrary disjoint
// ranges), but the two routes to it are unavailable on our Composio
// plan: (a) the curated `GOOGLESHEETS_BATCH_UPDATE` action expects a
// different schema and rejects native-format payloads, and (b)
// `composio.tools.proxyExecute` requires a paid feature flag on the
// API key (returns 403 ExternalProxy_OrgNotAllowed without it).
//
// Instead we pack cells into per-column CONTIGUOUS-ROW runs and emit
// one VALUES_UPDATE per run with a range like `Sheet1!B2:B21`. For a
// typical wave that writes whole columns this collapses 120 calls
// into 6 (one per output column) — well under quota for any
// realistic workflow shape (would only matter at ~60 distinct output
// columns per wave, which is atypical).
//
// Auth: composio-mcp listens on a private docker network and reads
// the acting user from `X-Proxy-User`. No service token (it's
// internal). The header value MUST be the goclaw-internal user UUID;
// composio maps it to the user's connected Google account.
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

// NewMCPSheetWriter constructs a composio-backed writer. composioURL
// is the URL of the composio-mcp sidecar; orgID is the tenant's
// external org id (kept for forward compatibility — composio
// identifies via X-Proxy-User only today).
func NewMCPSheetWriter(composioURL, _unusedLegacyToken, orgID string) *MCPSheetWriter {
	// _unusedLegacyToken: kept in the signature to avoid touching all
	// call sites in this PR. The old sheets-mcp X-Service-Token is
	// not sent anywhere — composio-mcp runs unauthenticated on the
	// docker internal network and uses X-Proxy-User for identity.
	return &MCPSheetWriter{
		composioURL: strings.TrimRight(composioURL, "/"),
		orgID:       orgID,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// BatchWrite implements SheetWriter. Groups cells into per-column
// contiguous row runs and issues one composio GOOGLESHEETS_VALUES_
// UPDATE call per run. Each run is retried with exponential backoff
// on Google Sheets rate-limit responses (HTTP 429 /
// RESOURCE_EXHAUSTED) so bursts above the per-user 60-write/min quota
// drain through instead of failing the whole wave. Non-rate-limit
// errors (auth, validation, network) fail-fast — per-cell retry at
// the orchestrator level is the right place to handle those.
func (w *MCPSheetWriter) BatchWrite(ctx context.Context, userID string, writes []CellWrite) error {
	if len(writes) == 0 {
		return nil
	}
	for _, run := range groupContiguousRuns(writes) {
		if err := w.writeRangeWithRetry(ctx, userID, run); err != nil {
			return err
		}
	}
	return nil
}

// writeRangeWithRetry wraps writeRange in a bounded exponential-
// backoff loop over rate-limit errors. Google's per-user write quota
// (default 60/min, rolling) is shared across all this user's sheet
// activity — under bursty loads a wave may temporarily exceed it.
// Retrying with backoff is Google's documented recommendation
// (https://developers.google.com/sheets/api/limits#error_codes).
//
// Backoff schedule (with up to 50% jitter per step): 1s, 2s, 4s, 8s,
// 16s. Total worst-case wait ≈ 31s + 5 actual call attempts. After
// the 5th failure we surface the underlying error so the orchestrator
// can mark the run as errored and the user sees a real failure
// rather than an indefinite hang.
//
// Context cancellation cuts the loop short — partial writes are
// retried on the next wave (orchestrator-level retry).
const writeRangeMaxAttempts = 5

func (w *MCPSheetWriter) writeRangeWithRetry(ctx context.Context, userID string, r cellRun) error {
	var lastErr error
	for attempt := 0; attempt < writeRangeMaxAttempts; attempt++ {
		err := w.writeRange(ctx, userID, r)
		if err == nil {
			return nil
		}
		if !isRateLimitError(err) {
			return err
		}
		lastErr = err
		// 1s, 2s, 4s, 8s, 16s with ±25% jitter so concurrent waves
		// from the same tenant don't synchronize on the retry tick.
		base := time.Duration(1<<attempt) * time.Second
		jitter := time.Duration(rand.Int63n(int64(base) / 2))
		wait := base + jitter - base/4
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("composio GOOGLESHEETS_VALUES_UPDATE rate-limited after %d attempts: %w", writeRangeMaxAttempts, lastErr)
}

// isRateLimitError matches Google Sheets API rate-limit signals as
// they surface through composio-mcp's response envelope. Three
// shapes seen in practice:
//
//   - HTTP 429 status code (rare — composio usually swallows it into
//     its envelope, but cover the case for defense-in-depth).
//   - Composio tool error envelope text containing "RESOURCE_EXHAUSTED"
//     (Google's canonical error code for write-quota exhaustion).
//   - Plain-language quota substrings ("Quota exceeded", "rate limit",
//     "quota metric 'Write requests'") that some Composio actions
//     emit instead of the canonical code.
//
// Auth / validation errors deliberately do NOT match — retrying those
// just wastes the backoff budget; the orchestrator surfaces them as
// run failures and the user sees the real cause.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "429") {
		return true
	}
	low := strings.ToLower(msg)
	for _, needle := range []string{
		"resource_exhausted",
		"quota exceeded",
		"rate limit",
		"ratelimit",
		"too many requests",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// cellRun is a contiguous block of cells in the SAME column with
// strictly-increasing row indices that differ by 1. Emitted as one
// VALUES_UPDATE per run with range `tab!{col}{startRow+2}:{col}{
// startRow+1+len(Values)}` and a column of values.
type cellRun struct {
	SpreadsheetID string
	SheetTab      string
	ColIdx        int
	StartRow      int
	Values        []string
}

// groupContiguousRuns partitions the cell write list into the minimum
// set of contiguous-row runs per (spreadsheet, tab, column). Cells in
// the same column with consecutive rows pack together; a gap (skipped
// row — e.g. one cell errored mid-wave) splits the run.
//
// Time: O(N log N) — dominated by the sort. N ≤ wave size, typically
// a few hundred. Allocates one slice per (col × contiguous-segment).
func groupContiguousRuns(writes []CellWrite) []cellRun {
	if len(writes) == 0 {
		return nil
	}
	// Sort by (spreadsheet, tab, col, row) so contiguous runs sit
	// next to each other in the sorted order.
	sorted := make([]CellWrite, len(writes))
	copy(sorted, writes)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.SpreadsheetID != b.SpreadsheetID {
			return a.SpreadsheetID < b.SpreadsheetID
		}
		if a.SheetTab != b.SheetTab {
			return a.SheetTab < b.SheetTab
		}
		if a.ColIdx != b.ColIdx {
			return a.ColIdx < b.ColIdx
		}
		return a.RowIdx < b.RowIdx
	})

	out := make([]cellRun, 0, len(sorted))
	var cur cellRun
	curOpen := false
	for _, c := range sorted {
		if curOpen &&
			c.SpreadsheetID == cur.SpreadsheetID &&
			c.SheetTab == cur.SheetTab &&
			c.ColIdx == cur.ColIdx &&
			c.RowIdx == cur.StartRow+len(cur.Values) {
			cur.Values = append(cur.Values, c.Value)
			continue
		}
		if curOpen {
			out = append(out, cur)
		}
		cur = cellRun{
			SpreadsheetID: c.SpreadsheetID,
			SheetTab:      c.SheetTab,
			ColIdx:        c.ColIdx,
			StartRow:      c.RowIdx,
			Values:        []string{c.Value},
		}
		curOpen = true
	}
	if curOpen {
		out = append(out, cur)
	}
	return out
}

// writeRange issues one VALUES_UPDATE for a contiguous column run.
// Range follows the header-offset convention: rowIdx 0 → sheet row 2.
// Values must be 2-D row-major; a single column maps to N rows × 1
// cell each: [["v1"], ["v2"], ...].
func (w *MCPSheetWriter) writeRange(ctx context.Context, userID string, r cellRun) error {
	tab := r.SheetTab
	if tab == "" {
		tab = "Sheet1"
	}
	col := colLetter(r.ColIdx)
	startA1 := fmt.Sprintf("%s%d", col, r.StartRow+2)
	endA1 := fmt.Sprintf("%s%d", col, r.StartRow+1+len(r.Values))
	a1 := fmt.Sprintf("%s!%s:%s", tab, startA1, endA1)

	values := make([][]any, 0, len(r.Values))
	for _, v := range r.Values {
		values = append(values, []any{v})
	}

	args := map[string]any{
		"spreadsheet_id":             r.SpreadsheetID,
		"range":                      a1,
		"values":                     values,
		"value_input_option":         "USER_ENTERED",
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
