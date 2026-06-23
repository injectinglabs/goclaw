---
name: parallel-research-sheet
description: |
  Build a DOWNLOADABLE spreadsheet (.xlsx) of N items where each row needs REAL, researched data (looked up on the web), FAST — using the `research_sheet` tool, which searches each item and extracts the columns for you in one call, then writing the returned rows to an .xlsx.

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
  version: "3.0.0"
---

# Parallel Research Sheet

Build a downloadable `.xlsx` of N researched rows FAST. The data lookup is done for you by the **`research_sheet` tool** — it web-searches EACH item and extracts the column values from live results, concurrently, and returns finished rows. You do NOT search or fill values yourself.

## ⚠️ Two hard rules
1. **Do NOT fill column values from memory.** Your recalled numbers/websites/locations are stale and wrong. The whole point of this skill is that `research_sheet` sources every value from live search. Never "correct" or "complete" the returned rows from prior knowledge.
2. **Do NOT spawn sub-agents** (`spawn`) for the lookups, and do NOT loop `web_search` one row at a time. `research_sheet` already fans the searches out concurrently in a single call — that's faster, cheaper, and more reliable.

## Recipe

1. **Decide the schema** — the exact column headers (e.g. `Firm, Website, HQ, Stage, Notable Investments`) and the total row count N. Put the row-key column (the item name) first.

2. **Get the item list (row keys).** If the N items are a well-known set, list them yourself; otherwise do ONE `web_search` to find the names. You need the N names before calling `research_sheet`.

3. **One `research_sheet` call.** Pass:
   - `items`: the array of N row keys (e.g. the firm names),
   - `columns`: the column headers to fill,
   - `context` (optional): a phrase to focus the searches, e.g. `"SF Bay Area seed-stage venture capital firm"`.
   It returns `{columns, rows}` where each row is real, search-derived data. (Up to 120 items per call; for N>120, call again for the rest.)

4. **Write + deliver.** Write ONE `.xlsx` via `exec` + openpyxl from the returned rows (row 1 = columns, data below, no banner/merged rows), validate ~N rows, then `deliver_file` it. Reply with a one/two-sentence summary — do NOT paste the data back as a markdown table (it renders as an interactive grid).

## Guardrails
- **`research_sheet`, not `spawn` and not memory** — this is the whole point.
- **Write exactly what the tool returns.** Blank cells the tool left empty stay blank — don't fill them in from recall.
- **Real data** — if `research_sheet` reports the search source is unavailable / returns all-blank rows, tell the user rather than inventing values.
- Small N (< ~20) or trivially-known data: skip the tool, build directly.

## Example — "top 100 VCs in the SF Bay Area"
1. Schema: `Firm, Website, HQ, Stage, Notable Investments`.
2. One `web_search` → list the 100 firm names.
3. ONE `research_sheet` with `items=[the 100 firm names]`, `columns=["Firm","Website","HQ","Stage","Notable Investments"]`, `context="SF Bay Area venture capital firm"`.
4. Take the returned `rows` → openpyxl `.xlsx` → `deliver_file`.
