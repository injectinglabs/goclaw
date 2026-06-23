---
name: parallel-research-sheet
description: |
  Build a DOWNLOADABLE spreadsheet (.xlsx) of N items where each row needs REAL, researched data (looked up via web_search), by fanning the research out across PARALLEL sub-agents so it's fast instead of sequential.

  Use this skill when ALL of these hold:
  - The user asks you to BUILD / MAKE / CREATE / GENERATE a spreadsheet, table, excel, or sheet of MANY items (≈20+ rows) — e.g. "build an excel of the top 100 VCs in the SF Bay Area", "make a sheet of the 50 biggest SaaS companies with their funding", "create a table of the top 100 companies by market cap with website + HQ + revenue".
  - Each row needs FACTUAL data you should LOOK UP (websites, locations, funding, prices, metrics, contacts), not invent from memory.
  - The deliverable is a file the user downloads in chat (an .xlsx that renders as an editable grid) — NOT a Google Sheet.

  Do NOT use this skill when:
  - The user has / wants a real Google Sheet enriched → use the `sheet-bulk-enrich` skill instead (that path writes cells into a Google Sheet via Composio).
  - The list is small (< ~20 rows) or the values are trivially well-known → just build it directly in one script; the parallel fan-out overhead isn't worth it.
  - The user only wants a quick markdown answer to read in chat.

  The whole point: REAL data + PARALLEL speed. Researching 100 rows one at a time is minutes of sequential web_search; fanning out ~10 parallel researchers cuts it to roughly the time of a single chunk.
metadata:
  author: injecting.ai
  version: "1.0.0"
---

# Parallel Research Sheet

Build a downloadable `.xlsx` of N researched rows FAST by splitting the research across parallel sub-agents (the `spawn` tool), each handling a disjoint chunk of rows, then merging into one file and delivering it.

## Why this is fast
goclaw executes the tool calls you emit **in a single turn concurrently**. So if you emit K `spawn` calls (`mode:"sync"`) in ONE turn, K sub-agents research their chunks **at the same time**, and each returns its rows inline. Wall-clock ≈ the slowest chunk, NOT the sum. Researching 100 rows sequentially ≈ minutes; 10 parallel chunks ≈ the time of ~10 rows.

## Recipe

1. **Decide the schema** — the exact column headers (e.g. `Rank, Firm, Website, HQ, Stage, Notable Investments`) and the total row count N the user asked for.

2. **Get the item list (the row keys).** If the N items are a well-known set you can name reliably, list them yourself. Otherwise do ONE `web_search` first to find the list of names. You need the N item names before you can fan out.

3. **Chunk** the N items into K groups of ~8–12 (so K ≈ N/10, capped at ~12). Give each chunk a DISJOINT slice — chunk 1 = items 1–10, chunk 2 = 11–20, … — so no two sub-agents research the same item.

4. **Fan out — in ONE turn, emit all K `spawn` calls** (this is what makes them run in parallel — do NOT spawn one chunk per turn). Each call:
   - `action: "spawn"`, `mode: "sync"`, `label: "rows 1-10"` (etc.)
   - `task:` *"Research these items and return ONE JSON array — one object per item — with EXACTLY these keys: <your columns>. Use web_search to find REAL values for each field; do NOT invent data, and if a value genuinely can't be found use an empty string. Items (in this exact order): <the 8–12 names for this chunk>. Return ONLY the JSON array — no prose, no markdown code fences."*

5. **Merge + write.** Once all K sub-agents return (inline, since `mode:"sync"`), concatenate their JSON arrays IN CHUNK ORDER, then write ONE `.xlsx` via `exec` + openpyxl: row 1 = the column headers, data immediately below, NO title/banner/merged rows. Validate you ended up with ~N rows.

6. **Deliver.** `deliver_file` the `.xlsx`. Then reply with a one- or two-sentence summary (what it contains + row count). Do NOT paste the data back as a markdown table — it already renders as an interactive grid.

## Guardrails
- **Disjoint chunks** — never hand two sub-agents the same items; split by the assigned slice.
- **Strict JSON out** — sub-agents must return an array of objects with identical keys so you can merge programmatically. If one returns junk or errors, re-spawn just that chunk.
- **Keep K ≤ ~12, chunk size ~8–12** — each sub-agent is a full research run, so too many tiny chunks adds more overhead than it saves.
- **Real data only** — the sub-agents must use web_search; if searches come back empty, tell the user the data source is unavailable rather than silently inventing values.
- **Small/known data** — for < ~20 rows or trivially-known values, skip the fan-out and build directly in one script.

## Example — "top 100 VCs in the SF Bay Area"
1. Schema: `Rank, Firm, Website, HQ, Stage, Notable Investments`.
2. One `web_search` → list the 100 firm names.
3. 10 chunks of 10. In ONE turn, emit 10 `spawn(mode:"sync")` calls: *"Research firms 1–10 … return a JSON array with keys [Firm, Website, HQ, Stage, Notable Investments] …"*, …, *"firms 91–100 …"*.
4. Merge the 10 arrays → 100 rows → openpyxl `.xlsx` → `deliver_file`.
