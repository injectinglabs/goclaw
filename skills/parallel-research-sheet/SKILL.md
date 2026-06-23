---
name: parallel-research-sheet
description: |
  Build a DOWNLOADABLE spreadsheet (.xlsx) of N items where each row needs REAL, researched data (looked up on the web), fast and accurate — using the `research_sheet` tool, which searches each item, extracts the full text of its pages, pulls emails/website/columns from live content, and delivers the .xlsx itself.

  Use this skill when ALL of these hold:
  - The user asks you to BUILD / MAKE / CREATE / GENERATE a spreadsheet, table, excel, or sheet of MANY items (≈20+ rows) — e.g. "build an excel of the top 100 VCs in the SF Bay Area with their emails", "make a sheet of the 50 biggest SaaS companies with funding", "table of the top 100 companies by market cap with website + HQ".
  - Each row needs FACTUAL data you should LOOK UP (emails, websites, locations, stage, focus), not invent from memory.
  - The deliverable is a file the user downloads in chat (an .xlsx that renders as an editable grid) — NOT a Google Sheet.

  Do NOT use this skill when:
  - The user has / wants a real Google Sheet enriched → use the `sheet-bulk-enrich` skill instead.
  - The list is small (< ~20 rows) or the values are trivially well-known → just build it directly.
  - The user only wants a quick markdown answer to read in chat.
metadata:
  author: injecting.ai
  version: "5.0.0"
---

# Parallel Research Sheet

Build a downloadable, web-researched `.xlsx` fast. The `research_sheet` tool does the heavy lifting: for each item it finds the official site, **extracts the full text** of its homepage + contact/about/team pages, pulls **emails by pattern-matching the real page content** (not snippets, not memory), resolves the website, fills the remaining columns from the extracted text, then **writes and delivers the `.xlsx` itself**.

## ⚠️ Two rules
1. **Use `research_sheet` — don't build the sheet by hand.** Do NOT write a Python/`exec` script to assemble rows: that fabricates data from memory. Do NOT fill column values yourself. `research_sheet` sources everything from live pages.
2. **Don't re-deliver.** `research_sheet` already sends the file. Do NOT call `deliver_file` afterward, and do NOT paste the data back as a markdown table.

## Recipe
1. **Decide the schema** — exact column headers, with the item-name column FIRST (e.g. `Firm, Website, Email, HQ, Stage, Notable Investments`). Columns named like `Website`/`Email` are filled deterministically; the rest come from page text.
2. **Get the item list (row keys).** If they're a well-known set, list them yourself; otherwise do ONE `web_search` to find the names. You need the N names before calling the tool.
3. **One `research_sheet` call.** Pass `items` (the N names), `columns` (headers, key first), optional `context` (e.g. `"SF Bay Area seed-stage venture capital firm"`) to focus the search, and optional `filename`. It researches all items in parallel and delivers the `.xlsx`.
4. **Done.** Reply with a one/two-sentence summary (e.g. "Delivered a 100-row sheet of SF Bay seed VCs with emails where published").

## Set expectations honestly
- **Emails:** real ones come only from firms that publish them on their site (`info@…`, a partner address, etc.). Many only have a contact form, so some Email cells will be blank — that's correct, not a failure. Never fill blanks from memory.
- **Coverage:** blanks mean nothing was found on the page. Don't "complete" them.
- For >150 items, call `research_sheet` again for the rest (it caps per call and tells you when it truncated).

## Example — "top 100 SF Bay Area seed VCs with emails"
1. Schema: `Firm, Website, Email, HQ, Stage, Notable Investments` (Firm first).
2. One `web_search` → list the 100 firm names.
3. ONE `research_sheet` with `items=[the 100 names]`, `columns=["Firm","Website","Email","HQ","Stage","Notable Investments"]`, `context="SF Bay Area seed-stage venture capital firm"`, `filename="top_100_seed_vcs_sf.xlsx"`.
4. It delivers the `.xlsx`. Reply with a one-line summary. Done.
