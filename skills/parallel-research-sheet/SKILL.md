---
name: parallel-research-sheet
description: |
  Build a DOWNLOADABLE spreadsheet (.xlsx) of N items where each row needs REAL, researched data (looked up on the web), FAST — by running all the lookups concurrently with the `batch_web_search` tool, then extracting the columns in one pass.

  Use this skill when ALL of these hold:
  - The user asks you to BUILD / MAKE / CREATE / GENERATE a spreadsheet, table, excel, or sheet of MANY items (≈20+ rows) — e.g. "build an excel of the top 100 VCs in the SF Bay Area", "make a sheet of the 50 biggest SaaS companies with their funding", "table of the top 100 companies by market cap with website + HQ + revenue".
  - Each row needs FACTUAL data you should LOOK UP (websites, locations, funding, prices, metrics, contacts), not invent from memory.
  - The deliverable is a file the user downloads in chat (an .xlsx that renders as an editable grid) — NOT a Google Sheet.

  Do NOT use this skill when:
  - The user has / wants a real Google Sheet enriched → use the `sheet-bulk-enrich` skill instead.
  - The list is small (< ~20 rows) or the values are trivially well-known → just build it directly.
  - The user only wants a quick markdown answer to read in chat.
metadata:
  author: injecting.ai
  version: "2.0.0"
---

# Parallel Research Sheet

Build a downloadable `.xlsx` of N researched rows FAST. The trick is to do all the web lookups **concurrently in a single `batch_web_search` call** — NOT one `web_search` at a time, and **NOT by spawning sub-agents**.

## ⚠️ Do NOT spawn sub-agents
Do **not** use the `spawn` tool for this. Spawning one agent per chunk runs a full LLM loop per chunk — it's slow (you wait on the slowest agent), wildly token-expensive, and over-searches. Use `batch_web_search` instead: the searches fan out as plain concurrent HTTP, with zero extra model turns.

## Recipe

1. **Decide the schema** — the exact column headers (e.g. `Firm, Website, HQ, Stage, Notable Investments`) and the total row count N.

2. **Get the item list (row keys).** If the N items are a well-known set, list them yourself; otherwise do ONE `web_search` to find the names. You need the N names before the batch lookup.

3. **One `batch_web_search` call.** Pass an array of N queries — **one focused query per item** that targets the columns you need, e.g. `"Sequoia Capital venture firm official website headquarters stage focus"`. All N searches run concurrently and come back together. (Up to 100 queries per call; for N>100, split into a few calls.)

4. **Extract in ONE pass.** From the combined results, pull the column values for every item — the top result URL is usually the official website; HQ/stage/focus come from the titles + snippets. If a value isn't in the results, use an empty string (don't go re-searching). Do this in your own turn — no extra tool calls per row.

5. **Write + deliver.** Write ONE `.xlsx` via `exec` + openpyxl (row 1 = headers, data below, no banner/merged rows), validate ~N rows, then `deliver_file` it. Reply with a one/two-sentence summary — do NOT paste the data back as a markdown table (it renders as an interactive grid).

## Guardrails
- **`batch_web_search`, not `spawn`** — this is the whole point. One concurrent batch call beats N sub-agents on speed, cost, and reliability.
- **One query per item**, focused on the columns you need. Don't issue multiple searches per item.
- **Extract from what you get** — if the batch results don't contain a value, leave it blank rather than firing more searches.
- **Real data** — if the batch comes back with no results at all, tell the user the search source is unavailable instead of inventing values.
- Small N (< ~20) or trivially-known data: skip the batch, build directly.

## Example — "top 100 VCs in the SF Bay Area"
1. Schema: `Firm, Website, HQ, Stage, Notable Investments`.
2. One `web_search` → list the 100 firm names.
3. ONE `batch_web_search` with 100 queries: `["Sequoia Capital venture firm website headquarters stage", "Andreessen Horowitz a16z website headquarters stage", … (100 total)]`.
4. Extract the 5 columns for all 100 firms from the combined results → openpyxl `.xlsx` → `deliver_file`.
