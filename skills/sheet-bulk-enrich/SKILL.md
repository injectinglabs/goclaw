---
name: sheet-bulk-enrich
description: |
  Fill multiple attributes for every item in a list, persisting the result to a Google Sheet. Use this skill whenever the user wants the SAME set of fields filled in for EACH of N items — regardless of whether they explicitly mention a Google Sheet, spreadsheet, or "enrichment". The pattern is "N items × M attributes", not the wording. If the sheet doesn't exist yet, this skill creates it and seeds the input column before running the parallel research subagents.

  Trigger phrasings to recognise (semantic match, not literal — paraphrases and translations into any language fire too):
  - Spreadsheet-explicit: "enrich my sheet", "fill column C for every row in B", "research each company in my spreadsheet", "for each ticker in A get price + news in B and C".
  - Table-implicit: "make/build/create a table with these N items and fill columns X, Y, Z for each", "compile data on these N items — for each give me A, B, C", "compare these N things by attributes A/B/C", "get X, Y, Z for each of <list>", "list N items with their X, Y, Z".

  Do NOT use this skill when: the user asks for a single answer (one company, one fact), a markdown table they just want to read in chat with no follow-up writes, or a sheet they want READ rather than filled. For a markdown-table reply to a small finite question, answer directly — do not run subagents.

  Heuristic: if you would otherwise need to (a) iterate over N items and (b) produce more than one attribute per item, this skill is correct. The user mentioning "table" / "таблица" without a sheet is a strong signal — assume they want a real persistent Google Sheet they can open, NOT a markdown blob in chat.
metadata:
  author: injecting.ai
  version: "3.0.0"
---

# Sheet Bulk Enrich

A playbook for filling many cells in a user's Google Sheet by fanning N research subagents out in parallel, collecting their JSON outputs, and committing every value in ONE optimized batch write. No dedicated orchestrator code — every step is a regular agent tool call, fully composable.

## When to trigger

Activate this skill any time the user wants the SAME M attributes filled in for EACH of N items — whether the sheet already exists or you need to create it first. The signal is "N × M", not the wording.

**Activate even when you already know the answers.** Public-domain data (NBA teams, S&P 500 companies, country capitals, common programming languages, famous people) is tempting to dump as a pre-filled CSV in one tool call. Do not do that — the user expects to see parallel research tasks running for each item, then the sheet appearing populated. A pre-filled CSV upload looks identical to the final file but loses the entire UX. ALWAYS run the spawn → wait → BULK_SHEET_WRITE pipeline below, even when the answer feels obvious. The data being "well-known" just means each subagent will finish quickly.

**Sheet-explicit phrasings** (user already has or wants a Google Sheet):
- "Enrich my sheet 'Q3 Prospects' — for each company find CEO, LinkedIn, last funding"
- "Fill column C with the LinkedIn URL for each person in column B"
- "Research every row in my spreadsheet and add summary in D"

**Table-implicit phrasings** (user just wants a table — but a real persistent Sheet, not chat-markdown):
- "Make/build/create a table with <list of N items> and fill columns X, Y, Z for each"
- "Compile data on <these N items>: X, Y, Z"
- "Compare <these N things> across X, Y, Z"
- "For each of <list>, give me X, Y, Z"

**Cross-language**: the skill matcher is multilingual. Paraphrases in other languages fire the same.

**Do NOT trigger when**: the user asks for a SINGLE answer (one company, one fact), a markdown table they want IN-CHAT with no follow-up actions, or a sheet they want READ rather than filled.

## Architecture (so you pick the right tools)

The pipeline is just three pure tool calls in sequence — no special server-side code:

1. **`mcp_composio_mcp__GOOGLEDRIVE_CREATE_FILE_FROM_TEXT`** — create the Sheet with header row only (no data), OR find an existing one via `GOOGLESHEETS_SEARCH_SPREADSHEETS`.
2. **`mcp_composio_mcp__BULK_SHEET_WRITE`** — seed the input column (item names in column A) in ONE call.
3. **`spawn`** (built-in tool) — fan out one subagent PER ROW; each researches its row's attributes and returns a small JSON.
4. **`spawn` action=`wait`** — block until every subagent completes; collect their JSON outputs.
5. **`mcp_composio_mcp__BULK_SHEET_WRITE`** — commit every (row, col) value in ONE optimized batch call.

`BULK_SHEET_WRITE` groups by column and writes contiguous-row ranges (one Google API call per column run, not per cell), with exponential-backoff retry on 429. That keeps you well under the per-user 60 writes/min Google quota even for 100-row sheets.

## Critical do-NOTs (read these first)

- **Do NOT use `GOOGLEDRIVE_CREATE_FILE_FROM_TEXT` with a pre-filled CSV body containing data rows.** A header-row-only seed is correct; embedding all the answers in the CSV defeats the parallel-research UX the user wants to see. Even if the data is public-domain, run subagents.
- **Do NOT loop `GOOGLESHEETS_VALUES_UPDATE` per cell.** That hits the 60/min quota and is N× slower. Use `BULK_SHEET_WRITE` instead — same Google account, fewer API calls.
- **Do NOT spawn subagents sequentially.** Issue all N `spawn` calls in ONE turn (one assistant message with N tool calls). The runtime fans them out in parallel; sequential spawning serializes the latency.
- **Do NOT skip the `wait` step.** You must call `spawn({action:"wait"})` to collect results before writing. Don't proceed on a hope that they finished.
- **Do NOT call `sheets_enrich_run`.** That is the legacy orchestrator path; the spawn-based pipeline below replaces it. If `sheets_enrich_run` still appears in the catalog, ignore it.

## Pipeline

### Step 1 — Sheet setup

If the user **already has** a Sheet:
- Get its `spreadsheet_id` from the URL (the long string between `/d/` and `/edit`) or by asking.
- Call `mcp_composio_mcp__GOOGLESHEETS_GET_SPREADSHEET_INFO` to confirm tab name + row/col counts.
- Call `mcp_composio_mcp__GOOGLESHEETS_VALUES_GET` on `Sheet1!1:1` to read the existing header row. Reuse those column letters in your schema below.

If the user **wants a new** Sheet:
- Call `mcp_composio_mcp__GOOGLEDRIVE_CREATE_FILE_FROM_TEXT` with `mime_type: "application/vnd.google-apps.spreadsheet"` and a CSV `content` that contains ONLY the header row, ending with a newline. Example: `"Company,CEO,LinkedIn,Last Funding\n"`. NO data rows.
- The response has `id` — that is the `spreadsheet_id`. Default tab name is `Sheet1`.

### Step 2 — Seed the input column (only if you created a new Sheet)

If the user supplied N items (e.g. `Apple, Microsoft, Google, ...`), populate column A in ONE call:

```json
{
  "tool": "mcp_composio_mcp__BULK_SHEET_WRITE",
  "arguments": {
    "spreadsheet_id": "<id>",
    "sheet_tab": "Sheet1",
    "cells": [
      {"row_idx": 0, "col_idx": 0, "value": "Apple"},
      {"row_idx": 1, "col_idx": 0, "value": "Microsoft"},
      {"row_idx": 2, "col_idx": 0, "value": "Google"}
    ]
  }
}
```

`row_idx` is 0-based (0 = first data row, lands on sheet row 2 because of the header). `col_idx` is 0-based (0 = A, 1 = B, …, 26 = AA).

### Step 3 — Confirm schema with the user

Show a one-line summary of the output columns and ask before running:

> Will fill these columns for each of 20 companies:
> - B (CEO) — current CEO full name
> - C (LinkedIn) — CEO's LinkedIn URL
> - D (Funding) — most recent funding round
>
> Sound right? (yes / changes)

Once they confirm, proceed.

### Step 4 — Spawn N research subagents

For EACH item (N items total), call `spawn`. Issue all N calls in ONE assistant turn so the runtime fans them out in parallel. Each call:

```json
{
  "tool": "spawn",
  "arguments": {
    "action": "spawn",
    "mode": "async",
    "label": "row-1-Apple",
    "task": "Research the company \"Apple Inc.\" and return STRICT JSON with these exact keys: {\"ceo\": <current CEO full name as string, or empty string if unknown>, \"linkedin\": <CEO's LinkedIn profile URL as string, or empty string>, \"funding\": <most recent funding round as 'Series X, $Y, YYYY-MM' or 'public', or empty string>}. Output ONLY the JSON object, no prose, no markdown fences."
  }
}
```

Conventions for `task` prompts:
- Embed each row's known data (the column-A value) directly in the prompt.
- Always say "STRICT JSON" + list the exact keys + state the allowed-types for each value.
- Always say "Output ONLY the JSON object, no prose, no markdown fences" — this is the single biggest determinant of parseable output.
- Allow empty string for unknown fields rather than `null` — string is easier to write to a cell.
- For research-heavy fields, the subagent can use `web_search` / `web_fetch`; you don't need to tell it which tools to use.

Conventions for `label`:
- Use `row-<sheet_row>-<short_item_name>` so the wait-result list is scannable. Example: `row-2-Apple`, `row-3-Microsoft`.

### Step 5 — Wait for every subagent to finish

After spawning, in the next turn issue:

```json
{ "tool": "spawn", "arguments": { "action": "wait", "timeout": 600 } }
```

`wait` blocks until every child of this agent completes (or `timeout` seconds elapse). The result is a formatted list, one line per task, with the task's full result text (capped at 4 KB per task).

If any task shows `[failed]`: include it in the user-facing summary but DON'T fail the whole batch — just skip its cells in the next step.

### Step 6 — Parse JSON outputs

For each completed task in the wait result, extract the JSON object. Be defensive:
- Strip any leading/trailing markdown fences (```json … ```) the model might have added despite instructions.
- If parsing fails for a row, treat all its fields as empty strings and note the row in the user summary.

### Step 7 — Build the cells array

One entry per (row, col) value. row_idx matches the row's 0-based data index (same numbering you used in Step 2). col_idx matches each output column you confirmed in Step 3.

```js
cells = []
for (i, result) in enumerate(results):
  cells.push({row_idx: i, col_idx: 1, value: result.ceo})       // B = col 1
  cells.push({row_idx: i, col_idx: 2, value: result.linkedin})  // C = col 2
  cells.push({row_idx: i, col_idx: 3, value: result.funding})   // D = col 3
```

### Step 8 — Commit all values in ONE call

```json
{
  "tool": "mcp_composio_mcp__BULK_SHEET_WRITE",
  "arguments": {
    "spreadsheet_id": "<id>",
    "sheet_tab": "Sheet1",
    "cells": [ /* all rows × all cols */ ]
  }
}
```

The tool packs cells by column and writes contiguous-row ranges (one API call per run, not per cell), retrying internally on 429 / RESOURCE_EXHAUSTED. Returns `{total_cells, ranges_written, failed_ranges: [{range, error}]}`. If `failed_ranges` is non-empty, retry just those ranges by mapping each range back to its `{row_idx, col_idx}` and calling `BULK_SHEET_WRITE` again with the failing cells.

### Step 9 — Report to user

One-line summary:

> Done — 20 rows × 3 columns enriched. Sheet: https://docs.google.com/spreadsheets/d/<id>. 2 rows had no source data (row 7, row 13).

Include the sheet URL. Mention any rows where the subagent couldn't find data.

## Sizing guidance

- **N (items)**: up to ~100 per run is fine; the runtime concurrency caps protect against fan-out abuse. For N > 100, batch into multiple skill invocations (50 at a time).
- **M (output cols)**: keep ≤ ~8 per row. Each output column is one field in the subagent's JSON return; too many fields per subagent makes the JSON brittle.
- **Wait timeout**: default 300s. For N > 30 or research-heavy tasks (deep web search per row), bump to 600s.

## Error handling

| Failure | Action |
|---|---|
| `GOOGLEDRIVE_CREATE_FILE_FROM_TEXT` returns auth error | Composio Google not connected. Tell user to connect Google in `/integrations`. |
| `BULK_SHEET_WRITE` returns non-empty `failed_ranges` | Retry just those cells in a second `BULK_SHEET_WRITE` call. |
| Subagent returns prose instead of JSON | Treat fields as empty strings for that row; note in user summary. |
| `wait` reports `[failed]` tasks | Include in user summary; skip their cells in `BULK_SHEET_WRITE`. |
| User cancels mid-run | Spawned subagents continue but commits don't happen. Report what would have run; user can re-invoke. |

## What this skill replaces (historical note)

Previous v2.x versions of this skill called `sheets_enrich_run` (an MCP tool that delegated to a dedicated server-side orchestrator). v3.x runs the same pattern as plain agent tool calls (`spawn` + `BULK_SHEET_WRITE`). Same UX, less specialized code path. The same approach generalizes to any "N items × M attributes" workload — bulk email, bulk Slack messages, bulk ticket classification, etc. — by swapping the final BULK_SHEET_WRITE for the appropriate sink tool.
