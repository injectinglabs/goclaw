---
name: sheet-bulk-enrich
description: Bulk-enrich rows in a user's Google Sheet — fill out one or more columns by running a single orchestrator workflow. Use when the user asks to "enrich my sheet", "research these companies in my spreadsheet", "fill in CEO/LinkedIn/funding for each row", "find data for every row in column A", or similar multi-row research-and-fill requests over a Google Sheet they have access to.
metadata:
  author: injecting.ai
  version: "2.0.0"
---

# Sheet Bulk Enrich

A playbook for filling many cells in a user's Google Sheet via the Sheet Workflows orchestrator. The orchestrator handles DAG planning, per-cell retry, batching, and live progress on its own — your job is to gather the schema, kick it off, and report results.

## When to trigger

Activate this skill when the user asks to populate or enrich data in a Google Sheet they own. Typical phrasings:
- "Enrich my sheet 'Q3 Prospects' — for each company find CEO, LinkedIn, last funding"
- "Fill column C with the LinkedIn URL for each person in column B"
- "Research every row in my spreadsheet and add summary in D"
- "For each ticker in column A, get current price + 1-line news in B and C"

If the user just wants to read a sheet or write a few cells, do NOT use this skill — use `mcp_composio_mcp__GOOGLESHEETS_VALUES_GET` / `GOOGLESHEETS_VALUES_UPDATE` directly.

## Architecture (so you pick the right tools)

The Sheet Workflows pipeline runs entirely on the user's existing Composio Google connection — no second OAuth prompt:

- **You (the agent)** — use composio's `GOOGLESHEETS_*` / `GOOGLEDRIVE_*` actions for sheet *setup* (create, search, append, get info).
- **`sheets_enrich_run`** (the one tool from sheets-mcp) — the *entry point* to the orchestrator. You build a column schema and call this once.
- **Orchestrator (server-side)** — reads the input range via composio, fans out cell-executor LLM calls in waves (DAG by `depends_on`), retries failures with exponential backoff, and writes cell values back through composio's `GOOGLESHEETS_VALUES_UPDATE`. Emits `workflow.event` over WS for live progress.

Never call sheets-mcp's old per-cell tools (status / read / update / batch_update) — those are retired. Use composio for ad-hoc cell ops, sheets_enrich_run for bulk.

## Prerequisites

Before starting, verify in this order:

1. **Composio Google connected** — call `mcp_composio_mcp__GOOGLESHEETS_GET_SPREADSHEET_INFO` on any test id to confirm. If composio returns "no connection", tell the user to connect Google in `/integrations` and STOP. Do NOT prompt for a separate sheets-mcp connect — that path is retired.
2. **Spreadsheet identified** —
   - If the user named the sheet, find it with `mcp_composio_mcp__GOOGLESHEETS_SEARCH_SPREADSHEETS`. If multiple match, ask which one.
   - If they pasted a URL, extract the spreadsheet_id (the long alphanumeric segment between `/d/` and the next `/`).
   - If they have no sheet yet ("create a new one with X"), create it with `mcp_composio_mcp__GOOGLEDRIVE_CREATE_FILE_FROM_TEXT` (mime `application/vnd.google-apps.spreadsheet`) or whatever Composio create action is available. Then add the header row + seed rows with `mcp_composio_mcp__GOOGLESHEETS_SPREADSHEETS_VALUES_APPEND`.
3. **Schema confirmed** — call `mcp_composio_mcp__GOOGLESHEETS_GET_SPREADSHEET_INFO` to learn tab names + row/col counts. Show the user a one-line summary (`Sheet1 — 47 rows × 6 cols`) and confirm the right tab + which column has the "key" data they want to enrich from.

## Building the column schema

For each output column the user wants filled, build a spec:
- **id** — short stable identifier (e.g. `ceo`, `linkedin`, `funding`).
- **name** — human-readable header that will appear in the sheet.
- **target_col** — A1 column letter the orchestrator writes to (e.g. `B`, `C`).
- **type** — `text` / `number` / `url` / `email`.
- **prompt** — one short sentence stating what to put in this column for ONE row, using the row's known data as input. Reference other columns by their id with `{ceo}`-style placeholders the orchestrator will substitute. Example: `"Given a company name in {Company}, return the current CEO's full name. Return ONLY the name, nothing else."`
- **depends_on** — list of other output column **ids** whose value this prompt needs. E.g. column `linkedin` depends on `["ceo"]` because finding the URL needs the name. The orchestrator topologically sorts waves by this.

Confirm the schema with the user before running. Show a compact table:

```
| id        | Header        | Col | Type | Depends on |
| ceo       | CEO           |  B  | text | (none)     |
| linkedin  | CEO LinkedIn  |  C  | url  | ceo        |
| funding   | Last funding  |  D  | text | (none)     |
```

## Execution

ONE call to `sheets_enrich_run` kicks the whole thing off. Example arguments:

```jsonc
{
  "user_id":        "<provided by goclaw>",
  "spreadsheet_id": "1AbcDEf...XyZ",
  "sheet_tab":      "Sheet1",
  "input_range":    "Sheet1!A2:A48",
  "workflow_name":  "Q3 Prospects — CEO + LinkedIn + funding",
  "columns": [
    { "id": "ceo",      "name": "CEO",          "target_col": "B", "type": "text",
      "prompt": "Given a company name in {Company}, return the current CEO's full name. Return ONLY the name.",
      "depends_on": [] },
    { "id": "linkedin", "name": "CEO LinkedIn", "target_col": "C", "type": "url",
      "prompt": "Given CEO name {ceo} and company {Company}, return the CEO's LinkedIn profile URL.",
      "depends_on": ["ceo"] },
    { "id": "funding",  "name": "Last funding", "target_col": "D", "type": "text",
      "prompt": "Given company {Company}, return the most recent funding round as 'Series X, $Y, YYYY-MM'.",
      "depends_on": [] }
  ]
}
```

The tool returns a `run_id` immediately. Cells fill in asynchronously over the next 30s–few minutes depending on row count.

## What happens server-side (so your reporting is accurate)

- Orchestrator reads `input_range` via composio, builds a DAG from `depends_on`, and runs in waves.
- Per cell: one LLM call (the tenant's chat provider) using the column `prompt` with row context substituted.
- Per cell write: one composio `GOOGLESHEETS_VALUES_UPDATE` call. Retries on transient failure (3 attempts, exp backoff).
- Live events: `workflow.event` on WS — `cell.started`, `cell.completed`, `cell.failed`, `wave.flushed`, `run.completed`.

## Reporting

After `sheets_enrich_run` returns:
- Tell the user the run kicked off and link the spreadsheet. Example: `Run started (run_id: …). 47 rows × 3 cols queued — watch the sheet, cells will fill in as the orchestrator works through them. I'll let you know when it's done.`
- If the user stays in chat, the WS events will surface in the SheetMessageBubble (live preview). If not, the final `run.completed` event will mark this turn done.
- On `run.completed`: one-line summary `Done — 47 rows × 3 columns enriched. 3 cells had no source (empty).` + 1-3 example rows.

## Error handling

| Failure | Action |
|---|---|
| `not_connected` from `sheets_enrich_run` | Composio Google not connected. STOP, tell user to connect Google in `/integrations`. |
| Empty rows in input_range | Tool returns with row_indices=[]. Tell the user no rows had a key value. |
| `run.failed` event with auth error | Composio token revoked. Same fix as above. |
| `cell.failed` events for some rows | Report them in the summary; offer "want me to retry the failed cells with a broader search?" |
| User cancels mid-run | Goclaw run will mark itself cancelled on next wave boundary; report what completed. |

## What to AVOID

- Do NOT call retired sheets-mcp tools (`sheets_status`, `sheets_get_connect_url`, `sheets_create_spreadsheet`, `sheets_append_rows`, `sheets_update_range`, `sheets_batch_update`, `sheets_set_cell_status`, `sheets_read_range`). They no longer exist. Use composio's `GOOGLESHEETS_*` and the one tool `sheets_enrich_run`.
- Do NOT loop manually issuing `GOOGLESHEETS_VALUES_UPDATE` per cell. Build the schema once, let the orchestrator do it.
- Do NOT spawn subagents per row yourself — the orchestrator already fans out and tracks each cell.
- Do NOT prompt the user to "connect Google Sheets" separately — Composio's Google connection is the single source of truth.
- Do NOT hallucinate values when sources are missing — the orchestrator returns empty for unreliable answers.
- Do NOT overwrite already-filled cells unless the user explicitly asked (orchestrator skips non-empty cells by default).
