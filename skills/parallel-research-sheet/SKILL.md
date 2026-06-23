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

3. **Chunk SMALL** — groups of **~5 items** (so K ≈ N/5). Small chunks are the whole game for cost/speed: each sub-agent is a full agent loop, so a chunk that takes ~6 tool calls is cheap and fast, while a chunk that needs 15+ blows up tokens (search results pile up in its context every turn) AND wall-clock (you wait for the slowest worker). Give each chunk a DISJOINT slice (chunk 1 = items 1–5, chunk 2 = 6–10, …). It's fine to have ~20 small sub-agents — they run concurrently.

4. **Fan out — in ONE turn, emit all K `spawn` calls** (this is what makes them run in parallel — do NOT spawn one chunk per turn). Each call:
   - `action: "spawn"`, `mode: "sync"`, `label: "rows 1-5"` (etc.)
   - `task:` *"Research these 5 items and return ONE JSON array — one object per item — with EXACTLY these keys: <your columns>. Be LEAN and FAST: run AT MOST one web_search per item, take the values straight from those results, and return immediately. Do NOT web_fetch, do NOT re-search or second-guess, do NOT verify across sources, do NOT reason at length — finish in as few tool calls as possible. If a value isn't in the search results, use an empty string rather than digging further. Items: <the 5 names for this chunk>. Return ONLY the JSON array — no prose, no markdown fences."*

   Each sub-agent is hard-capped at 20 internal iterations — keeping chunks to ~5 items + one-search-per-item keeps every worker well under that, which is what makes the run cheap and fast.

5. **Merge + write.** Once all K sub-agents return (inline, since `mode:"sync"`), concatenate their JSON arrays IN CHUNK ORDER, then write ONE `.xlsx` via `exec` + openpyxl: row 1 = the column headers, data immediately below, NO title/banner/merged rows. Validate you ended up with ~N rows.

6. **Deliver.** `deliver_file` the `.xlsx`. Then reply with a one- or two-sentence summary (what it contains + row count). Do NOT paste the data back as a markdown table — it already renders as an interactive grid.

## Guardrails
- **Disjoint chunks** — never hand two sub-agents the same items; split by the assigned slice.
- **Strict JSON out** — sub-agents must return an array of objects with identical keys so you can merge programmatically. If one returns junk or errors, re-spawn just that chunk.
- **Keep chunks SMALL (~5 items)** and sub-agents LEAN (one search per item, extract, return — no web_fetch / re-search / verify / long reasoning). A heavy 10–15-item chunk makes a sub-agent loop ~20 times, and 10 of those cost millions of tokens and minutes of wall-clock. Many small lean chunks (~20 of them) run concurrently and stay cheap. This is the difference between a ~$0.x sheet and a multi-dollar one.
- **Real data only** — the sub-agents must use web_search; if searches come back empty, tell the user the data source is unavailable rather than silently inventing values.
- **Small/known data** — for < ~20 rows or trivially-known values, skip the fan-out and build directly in one script.

## Example — "top 100 VCs in the SF Bay Area"
1. Schema: `Rank, Firm, Website, HQ, Stage, Notable Investments`.
2. One `web_search` → list the 100 firm names.
3. **20 chunks of 5**. In ONE turn, emit 20 `spawn(mode:"sync")` calls: *"Research firms 1–5 — one web_search each, extract [Firm, Website, HQ, Stage, Notable Investments], return JSON now"*, …, *"firms 96–100 …"*. They run concurrently.
4. Merge the 20 arrays → 100 rows → openpyxl `.xlsx` → `deliver_file`.
