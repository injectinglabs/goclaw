---
name: parallel-research-sheet
description: |
  Build a DOWNLOADABLE spreadsheet (.xlsx) of N items where each row needs REAL, researched data (looked up on the web) — using the `research_sheet` tool, which web-searches each item, extracts the columns, writes the .xlsx, and delivers it to the user, all in one call.

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
  version: "4.0.0"
---

# Parallel Research Sheet

Build a downloadable, web-researched `.xlsx` of N rows. The `research_sheet` tool does everything: it web-searches EACH item concurrently, extracts the column values from live results, writes the `.xlsx`, and delivers the download link to the user — in a single call.

## ⚠️ While this skill is active, `exec`/`bash`/`write_file` are disabled
That's intentional. Those are the tools you'd otherwise use to fabricate data via a Python script — which produces wrong, made-up values. The ONLY way to produce the sheet is `research_sheet`, which sources every value from real search. Do not look for another way; there isn't one, by design.

## Recipe

1. **Decide the schema** — the exact column headers (e.g. `Firm, Website, HQ, Stage, Notable Investments`). Put the row-key column (the item name) FIRST.

2. **Get the item list (row keys).** If the N items are a well-known set, list them yourself; otherwise do ONE `web_search` to find the names. You need the N names before calling `research_sheet`.

3. **One `research_sheet` call.** Pass:
   - `items`: the array of N row keys (e.g. the firm names),
   - `columns`: the column headers (item-name column first),
   - `context` (optional): a phrase to focus the searches, e.g. `"SF Bay Area seed-stage venture capital firm"`,
   - `filename` (optional): e.g. `"top_100_seed_vcs_bay_area.xlsx"`.
   The tool searches every item, fills the columns from real results, writes the `.xlsx`, and **delivers it to the user itself**.

4. **Done — do NOT call `deliver_file`** and do NOT try to rewrite or "complete" the file. `research_sheet` already delivered it. Reply with a one/two-sentence summary (e.g. "Delivered a 100-row sheet of SF Bay Area seed VCs"). Do NOT paste the data back as a markdown table.

## Guardrails
- **`research_sheet` is the only path** — `exec`/`write_file` are off; don't fight it.
- **Blanks are correct.** Where search didn't support a value, the cell is empty. That's honest — never fill blanks from memory.
- **Real data** — if `research_sheet` reports the search source is unavailable, tell the user rather than inventing values.
- For >120 items, call `research_sheet` again for the remainder (it caps at 120 per call and tells you when it truncated).
- Small N (< ~20) or trivially-known data: this skill isn't needed; build directly.

## Example — "top 100 VCs in the SF Bay Area"
1. Schema: `Firm, Website, HQ, Stage, Notable Investments` (Firm first).
2. One `web_search` → list the 100 firm names.
3. ONE `research_sheet` with `items=[the 100 firm names]`, `columns=["Firm","Website","HQ","Stage","Notable Investments"]`, `context="SF Bay Area venture capital firm"`, `filename="top_100_vcs_bay_area.xlsx"`.
4. It delivers the `.xlsx`. Reply with a one-line summary. Done.
