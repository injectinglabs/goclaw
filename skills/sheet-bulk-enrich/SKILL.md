---
name: sheet-bulk-enrich
description: Bulk-enrich rows in a user's Google Sheet — fill out one or more columns by researching each row in parallel. Use when the user asks to "enrich my sheet", "research these companies in my spreadsheet", "fill in CEO/LinkedIn/funding for each row", "find data for every row in column A", or similar multi-row research-and-fill requests over a Google Sheet they have access to.
metadata:
  author: injecting.ai
  version: "1.0.0"
---

# Sheet Bulk Enrich

A playbook for filling many cells in a user's Google Sheet by researching each row independently and writing results back, in parallel where possible.

## When to trigger

Activate this skill when the user asks to populate or enrich data in a Google Sheet they own. Typical phrasings:
- "Enrich my sheet 'Q3 Prospects' — for each company find CEO, LinkedIn, last funding"
- "Fill column C with the LinkedIn URL for each person in column B"
- "Research every row in my spreadsheet and add summary in D"
- "For each ticker in column A, get current price + 1-line news in B and C"

If the user just wants to read a sheet or write a few cells, do NOT use this skill — use `sheets_read_range` / `sheets_update_range` directly.

## Prerequisites

Before starting, verify in this order:

1. **Google Sheets connected** — call `sheets_status`. If `connected: false`, ask the user to connect via `sheets_get_connect_url` and STOP.
2. **Spreadsheet identified** — if the user named the sheet ("Q3 Prospects"), find it with `drive_list_files` filtered by `mime_type='application/vnd.google-apps.spreadsheet'` and matching name. If multiple match, ask which one. If they pasted a URL, extract the spreadsheet_id (the long alphanumeric segment between `/d/` and the next `/`).
3. **Schema confirmed** — call `sheets_get_spreadsheet` to learn tab names + row/col counts. Show the user a one-line summary (`Sheet1 — 47 rows × 6 cols`) and confirm the right tab + which column has the "key" data they want to enrich from.

## Building the column schema

For each output column the user wants filled, build a small spec:
- **target_range** in A1 (e.g. `B2:B`)
- **prompt** — one short sentence stating what to put in this column for ONE row, using the row's known data as input. Example: `"Given a company name in {Company}, return the current CEO's full name. Return ONLY the name, nothing else."`
- **type** — text / number / url / email
- **depends_on** — list of other output columns whose value this prompt needs. E.g. "CEO LinkedIn" depends on "CEO" because finding the URL needs the name.

Confirm the schema with the user before running. Show a compact table:

```
| Col | Header        | Type | Depends on    |
| B   | CEO           | text | Company (A)   |
| C   | CEO LinkedIn  | url  | CEO (B)       |
| D   | Last funding  | text | Company (A)   |
```

## Execution

### Read input data

Read the entire key column + any pre-existing data with `sheets_read_range` in ONE call (e.g. `Sheet1!A2:Z`). Identify rows that have a key value but lack one or more target columns.

### Group output columns into waves

Use `depends_on` to compute waves:
- Wave 1: columns that depend on nothing
- Wave 2: columns whose dependencies are all in Wave 1
- … etc

Within a wave, every column can be filled independently for every row.

### Per-cell execution

For each (row × column) cell to fill:

1. **Mark as in-progress** with `sheets_set_cell_status(sheet_id, row_idx, col_idx, note="⏳ enriching…")` BEFORE doing work.
2. **Research** — use `web_search` and `web_fetch` to find the answer. Use the row's known data (other column values) plus the column prompt as context. Cite at least one source URL you used.
3. **Format the result** strictly per the column type:
   - text: a plain string, no markdown
   - number: a numeric value (parseable by Sheets)
   - url: a full https:// URL
   - email: an RFC-valid email address
4. If you cannot find a reliable answer, return an empty string and clear the status note. Do NOT hallucinate.

### Parallel fanout

For each wave, spawn subagents — ONE per row — that handle ALL columns in that wave for THAT row. Each subagent does its own research + cell writes. The orchestrator (this top-level run) waits for the wave to complete before moving to the next.

If the sheet has >20 rows, throttle the fanout: spawn at most 20 subagents concurrently. Use the subagent barrier between batches.

### Batching writes

When subagents return values, DO NOT write each cell individually — that burns Google's 60/min/user quota. Collect 30-50 cell updates into ONE `sheets_batch_update` call and flush. Flush after every wave completes regardless of count.

### Status markers

Always:
- BEFORE a cell starts: ⏳ note
- AFTER success: clear the note + write the value
- AFTER error: `✗ <terse reason>` note (e.g. `✗ no source found` / `✗ rate limited`)

The user opens the Google Sheet and sees live progress. This is the killer UX.

## Error handling

| Failure | Action |
|---|---|
| `auth_failed` from any sheets_* tool | STOP, tell user to reconnect Google Sheets via `sheets_get_connect_url`. |
| Rate-limited (Google quota) | Pause 60s, then retry. If repeated, lower batch size and stop fanning out new rows until existing ones finish. |
| No source found for a cell | Empty string + clear status. Do NOT make up data. |
| User cancels mid-run | Finish the in-flight wave's cells (so the sheet is consistent), then stop. Report what was completed. |

## Reporting

After the run completes:
- One-line summary: `Done — 47 rows × 3 columns enriched. 3 cells had no source.`
- Show 1-3 example rows of what landed
- Offer a follow-up: "Want me to retry the empty cells with a broader search?"

## What to AVOID

- Do NOT write all cells via individual `sheets_update_range` calls — always batch
- Do NOT skip the ⏳ status markers — they're the live-progress UX
- Do NOT hallucinate values when sources are missing — empty cell + status note instead
- Do NOT include explanatory prose in cell values — agents return raw structured values, the column type dictates shape
- Do NOT overwrite already-filled cells unless the user explicitly asked
- Do NOT proceed if Google Sheets is not connected — surface the reconnect link and stop
