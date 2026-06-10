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
  version: "3.4.1"
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

One pipeline, six tool calls:

1. `mcp_composio_mcp__GOOGLEDRIVE_CREATE_FILE_FROM_TEXT` — create Sheet with header row only.
2. `mcp_composio_mcp__BULK_SHEET_WRITE` — seed column A with item names.
3. `spawn` N ULTRA-LIGHTWEIGHT subagents in ONE turn — each does EXACTLY ONE `web_search` call then returns JSON immediately. No iteration, no bash, no web_fetch, no second search.
4. `spawn({action:"wait"})` — block until all return.
5. Parse JSONs, build cells array.
6. `mcp_composio_mcp__BULK_SHEET_WRITE` — commit every (row, col) cell in one optimized call.

The parallel-subagent fan-out is the visible UX — the user expects to see N research chips. The "one search per subagent" rule is what keeps it fast: each subagent costs ~3-8 K tokens and ~5-15 seconds. For 25 rows in parallel, wall-clock is ~15-20s total, tokens ~100-200 K.

`BULK_SHEET_WRITE` groups by column and writes contiguous-row ranges (one Google API call per column run, not per cell), with exponential-backoff retry on 429. Keeps you under Google's 60 writes/min quota even for 100-row sheets.

## Critical do-NOTs (read these first)

- **Do NOT call `web_search` or `web_fetch` at the parent level for the LIST of items.** The user asked for famous well-known entities (top-25 unicorns, NBA teams, S&P 500, etc.) — your training already knows these names. List them inline in your thinking step and move on. If you preflight-search the list, you'll get rate-limited or stuck on provider errors and the entire run aborts before the Sheet is even created. Parent-level web_search is RESERVED for custom user-supplied items.
- **Do NOT abort the run on web_search error at the parent level.** If you accidentally did preflight-search and it errored, ignore the error completely and proceed with the list you already know. Subagents handle their own web_search internally.
- **Do NOT use `GOOGLEDRIVE_CREATE_FILE_FROM_TEXT` with a pre-filled CSV body containing data rows.** Header-row-only seed is correct; embedding all answers in CSV defeats the BULK_SHEET_WRITE step and skips parallel-subagent visibility. Always seed column A first, then spawn N subagents, then BULK_SHEET_WRITE.
- **Do NOT loop `GOOGLESHEETS_VALUES_UPDATE` per cell.** Hits the 60/min quota and is N× slower. Use `BULK_SHEET_WRITE` instead.
- **Do NOT skip spawn mode.** Even when items are well-known to you (NBA, top companies, unicorns) — spawn anyway. The user wants to SEE N parallel research chips. That's the UX. Trust the constraint block to keep each subagent cheap (~3-8 K tokens).
- **Do NOT spawn subagents sequentially.** Issue all N `spawn` calls in ONE assistant turn (one message with N tool calls). The runtime fans them out in parallel; sequential spawning serializes the wall-clock.
- **Do NOT re-spawn a row that has already been spawned.** If you called `spawn` for `row-2-ByteDance` once, you have already issued that task. Do NOT call `spawn` again for the same label even if `spawn({action:"list"})` shows it as completed — the result is already in the system, you'll collect it via `wait`. Re-spawning the same labels burned 9 extra spawns on the v3.3.x test, eating the iteration budget so the final `BULK_SHEET_WRITE` never fired. ONE spawn per row, period.
- **Do NOT verify or review after `wait` — go DIRECTLY to BULK_SHEET_WRITE.** When `wait` returns, your VERY NEXT tool call MUST be `BULK_SHEET_WRITE` with the full cells array. Do NOT enter a thinking block titled "Reviewing Task Outcomes" or "Verifying Data" — the data is whatever the subagents returned, and your job is to commit it as-is. Empty subagent fields become empty cells. Wrong-looking values can be fixed in a follow-up turn. The fastest path from `wait` to user-visible Sheet is ONE tool call; any thinking step in between risks eating the remaining iteration budget.
- **Do NOT weaken the HARD CONSTRAINT block in Step 4's task prompt.** Each subagent MUST do exactly one web_search and stop. Without that block, subagents iterate 10-20× and burn 50-200 K tokens each on slop-loops.
- **Do NOT skip the `wait` step.** You must call `spawn({action:"wait"})` to collect results before writing.

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

### Step 3 — Decide the schema and the input list. NEVER preflight-search.

Decide the output columns from the user's request and proceed to Step 4 in the SAME assistant turn — do NOT pause to ask "are these columns right?". Mention the schema you chose in your final summary so the user can see it.

**ABSOLUTELY DO NOT call `web_search`, `web_fetch`, or any other tool to find the LIST of items at the parent level.** The user asked for famous well-known entities (top unicorns, NBA teams, S&P 500, country capitals, top universities — pick whatever fits the request). You already know these names from your training data. Write them down directly in your thinking step. The list does not need verification — the user did not ask for "verified live list", they asked for "give me 25 items + fill these columns".

If you find yourself thinking "let me search to confirm the top 25" — STOP. You're wasting tokens and time. Your training knows the top 25 unicorns. List them inline:
- OpenAI, SpaceX, ByteDance, Anthropic, Ant Group, Stripe, Databricks, Shein, xAI, Canva, Revolut, Epic Games, CoreWeave, Fanatics, Chime, Discord, Plaid, Wiz, Scale AI, Figma (or its current independent state), Miro, Rippling, Devoted Health, Faire, Grammarly — pick any 25, the user will tell you if they want different ones.

For NBA: list the 30 teams from memory. For S&P 500 top-25: list them. For Russian / Chinese / regional lists: same — your training knows them.

**Web_search at the parent level is RESERVED for: a list of CUSTOM user-supplied items (e.g. "fill data for these 50 prospects I just pasted") OR if the user explicitly asked to "find latest top-25 from a fresh source".** Otherwise SKIP preflight searches entirely.

You will ALWAYS use spawn mode below (parallel subagents, one per row). The user wants to SEE parallel research chips for each row. Do not collapse this into one big assistant call — the visible parallel execution IS the deliverable.

If `web_search` errors at the parent (e.g. provider timeout), DO NOT retry and DO NOT abort the run. Proceed without it — you have the list in your head already. Move straight to Step 4 (spawn).

### Step 4 — Spawn N ULTRA-LIGHTWEIGHT subagents (training-only, no tools)

For EACH item (N items total), call `spawn`. Issue all N calls in ONE assistant turn so the runtime fans them out in parallel.

**Each subagent answers PURELY from its training knowledge and outputs JSON immediately — NO tool calls at all.** For the well-known entities this skill handles (top public companies, top unicorns, NBA teams, country capitals, top universities, S&P 500, famous people, programming languages), the subagent's training data has every field. Web search is unreliable on stage right now (DDG blocks our IP) and not needed for known facts. Subagents that pause to web_search just slow the batch and return empty when DDG times out — both bad outcomes. Skip the search entirely.

The constraint block below MUST be pasted into every spawn task verbatim — do not paraphrase, do not edit, do not trim. It is the single most important thing in this skill.

```json
{
  "tool": "spawn",
  "arguments": {
    "action": "spawn",
    "mode": "async",
    "label": "row-2-Apple",
    "task": "Return STRICT JSON for the company \"Apple Inc.\" with the keys the parent agent asked for (example: {\"ceo\": \"<full name>\", \"linkedin\": \"<URL>\", \"funding\": \"<Series X, $Y, YYYY-MM or 'public'>\"}).\n\nHARD CONSTRAINTS — these are absolute, no exceptions:\n\n1. FILL EVERY FIELD FROM YOUR TRAINING KNOWLEDGE. Apple's CEO is Tim Cook. Apple is public. Apple's HQ is Cupertino, USA, founded 1976. These are facts you already know — write them directly. Same logic for any well-known unicorn, NBA team, top company, country capital, language, university.\n\n2. DO NOT call ANY tools. NO web_search. NO web_fetch. NO bash. NO write_file. ZERO tool calls. Your ENTIRE job is to output one JSON object from what you already know.\n\n3. If a field is truly unknown (e.g. precise dollar-amount of a private company's last round from 6 months ago that's not stable in your training), use empty string \"\" or \"нет данных\". Use this for the field, not for the whole object. NEVER return all-empty JSON for a famous company you can describe in plain language — that's a knowledge failure, not a data-source failure.\n\n4. Output ONLY the JSON object on its own line. No prose. No markdown fences. No commentary. The first character of your final response MUST be `{`. One line of JSON, then done.\n\nYou have 25 siblings running in parallel — your job is fast (~5 sec, no tool calls) and complete (every field filled from training)."
  }
}
```

Conventions:
- Embed each row's value (the column-A entry) directly in the prompt where the example shows "Apple Inc.".
- Use `label = row-<sheet_row>-<short_item_name>` so the wait-result list is scannable.
- Match the JSON keys to the column schema the user asked for. Example above uses ceo/linkedin/funding; a different prompt might use country/founded/last_round/lead_investor/industry/product.
- The "no tools" rule is THE thing. Subagents that call web_search hit DDG, time out, return empty — and the cell is blank. Subagents that just answer from training fill the row. Force-disable the tool path via the prompt.

**Expected per-subagent cost: ~1-3 K tokens (one LLM turn, zero tool latency), ~3-8 seconds.** For 25 rows in parallel, total wall-clock ~8-15 s, total tokens ~30-80 K — much cheaper than the v3.3.x "one search each" path.

### Step 5 — Wait for every subagent to finish

After spawning, in the next turn issue ONE tool call — `spawn` with action `wait`:

```json
{ "tool": "spawn", "arguments": { "action": "wait", "timeout": 600 } }
```

`wait` blocks until every child of this agent completes (or `timeout` seconds elapse). The result is a formatted list, one line per task, with the task's full result text (capped at 4 KB per task).

**Do NOT call `spawn({action:"list"})` to check on progress.** The `wait` already blocks until completion — `list` adds an iteration for zero new information.

**Do NOT re-spawn rows that `list` shows as completed.** "Completed" means the result is already collected; the next `wait` will include it. Re-spawning the same label is the single biggest tool-budget waste — it cost an entire test run on v3.3.x.

If any task shows `[failed]`: include the row in the final summary but DON'T fail the batch — write an empty cell for it in Step 7.

### Step 6 — Parse JSON outputs AND immediately commit (combined step — no thinking break)

In the SAME assistant message that received the `wait` result, do all of this:

1. Parse each task's JSON. Be defensive — strip ```json fences if present. On parse failure, treat the row's fields as empty strings.
2. Build the cells array (Step 7 below).
3. Call `BULK_SHEET_WRITE` (Step 8 below).

That is ONE assistant turn with ONE tool call (BULK_SHEET_WRITE). Do NOT enter a thinking block titled "Reviewing Task Outcomes" or "Cross-checking Data" — that wastes the remaining iteration budget. The data is whatever the subagents returned; commit it as-is. The user can ask to re-run for specific rows after seeing the result.

### Step 7 — Build the cells array

One entry per (row, col) value from the parsed JSONs. row_idx matches the row's 0-based data index (same numbering you used in Step 2). col_idx matches each output column from your schema.

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

## Generalises beyond Sheets

The pattern `spawn N research subagents → wait → bulk-commit tool` works for any "N items × M attributes" workload — bulk email (final commit: `mcp_composio_mcp__GMAIL_SEND_EMAIL` per result), bulk Slack messages (`SLACK_CHAT_POST_MESSAGE`), bulk ticket classification (the appropriate sink), bulk Notion page creation (`NOTION_CREATE_NOTION_PAGE`), etc. Only the final commit-tool changes; the spawn pipeline stays identical.
