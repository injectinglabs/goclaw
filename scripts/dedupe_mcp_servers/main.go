// scripts/dedupe_mcp_servers/main.go
//
// One-shot admin script: collapses identical per-tenant MCP server rows into a
// single global row (tenant_id IS NULL), eliminating the per-tenant duplicates
// that were created by provisionStandardMCPServers.
//
// A group of rows for the same name is consolidated when ALL of these are true:
//   1. No global row (tenant_id IS NULL) already exists for that name.
//   2. Every row in the group has identical config fields after decryption:
//      transport, command, args, url, headers, env, api_key, tool_prefix,
//      timeout_sec, settings, enabled.
//
// Consolidation picks the oldest row as canonical (preserves its UUID and
// grants), sets its tenant_id to NULL, re-homes every other row's grants to the
// canonical ID, then deletes the duplicate rows (ON DELETE CASCADE cleans up any
// remaining grants that would conflict).
//
// Skipped groups (config mismatch or already global) are reported but not
// modified.
//
// Usage:
//
//	go run ./scripts/dedupe_mcp_servers --dry-run                          # print plan only
//	go run ./scripts/dedupe_mcp_servers --apply                            # apply (no backup)
//	go run ./scripts/dedupe_mcp_servers --apply --backup-to=backup.json    # backup first, then apply
//
// Required env: GOCLAW_POSTGRES_DSN
// Optional env: GOCLAW_ENCRYPTION_KEY (needed to compare encrypted fields)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nextlevelbuilder/goclaw/internal/crypto"
)

// --------------------------------------------------------------------------
// Backup types (raw DB rows captured before any writes)
// --------------------------------------------------------------------------

type backupBundle struct {
	CapturedAt      time.Time                  `json:"captured_at"`
	MCPServers      []map[string]any           `json:"mcp_servers"`
	AgentGrants     []map[string]any           `json:"mcp_agent_grants"`
	UserGrants      []map[string]any           `json:"mcp_user_grants"`
	UserCredentials []map[string]any           `json:"mcp_user_credentials"`
	AccessRequests  []map[string]any           `json:"mcp_access_requests"`
}

// --------------------------------------------------------------------------
// Data types
// --------------------------------------------------------------------------

type serverRow struct {
	ID          string
	Name        string
	DisplayName string
	Transport   string
	Command     string
	Args        []byte
	URL         string
	Headers     []byte // may be AES-GCM encrypted JSON string
	Env         []byte // may be AES-GCM encrypted JSON string
	APIKey      string // may be AES-GCM encrypted string
	ToolPrefix  string
	TimeoutSec  int
	Settings    []byte
	Enabled     bool
	CreatedBy   string
	CreatedAt   time.Time
	TenantID    *string // nil == global
}

// serverConfig holds the fields that must be identical for a group to be
// consolidated. Sensitive fields are stored decrypted.
type serverConfig struct {
	Transport  string
	Command    string
	Args       any // unmarshalled JSON
	URL        string
	Headers    any // unmarshalled JSON
	Env        any // unmarshalled JSON
	APIKey     string
	ToolPrefix string
	TimeoutSec int
	Settings   any // unmarshalled JSON
	Enabled    bool
}

type groupAction struct {
	Name         string
	CanonicalID  string // oldest row kept as global
	DuplicateIDs []string
	Skip         bool
	SkipReason   string
	GrantCounts  map[string]grantCount // dup_id → counts
}

type grantCount struct {
	AgentGrants     int
	UserGrants      int
	UserCredentials int
	AccessRequests  int
}

// --------------------------------------------------------------------------
// Main
// --------------------------------------------------------------------------

func main() {
	dryRun := flag.Bool("dry-run", false, "Print plan without executing")
	apply := flag.Bool("apply", false, "Execute consolidation")
	backupTo := flag.String("backup-to", "", "Path to write JSON backup before applying (recommended with --apply)")
	flag.Parse()

	if !*dryRun && !*apply {
		fmt.Fprintln(os.Stderr, "Usage: --dry-run or --apply [--backup-to=FILE]")
		os.Exit(1)
	}
	if *dryRun && *apply {
		fmt.Fprintln(os.Stderr, "Specify exactly one of --dry-run or --apply")
		os.Exit(1)
	}

	dsn := os.Getenv("GOCLAW_POSTGRES_DSN")
	if dsn == "" {
		log.Fatal("GOCLAW_POSTGRES_DSN is required")
	}
	encKey := os.Getenv("GOCLAW_ENCRYPTION_KEY")
	if encKey == "" {
		fmt.Fprintln(os.Stderr, "WARNING: GOCLAW_ENCRYPTION_KEY not set — encrypted fields (headers/env/api_key) will be compared as raw bytes (may cause false mismatches)")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	ctx := context.Background()

	actions, err := analyzeAll(ctx, db, encKey)
	if err != nil {
		log.Fatalf("analyze: %v", err)
	}
	if len(actions) == 0 {
		fmt.Println("No duplicate MCP server names found. Nothing to do.")
		return
	}

	// Print plan regardless of mode.
	printPlan(actions)

	if *dryRun {
		fmt.Println("\n[dry-run] No changes made.")
		return
	}

	// --apply mode: optionally backup first, then execute each consolidation.

	// Collect all IDs that will be touched (canonical + duplicates).
	var allServerIDs []string
	for _, a := range actions {
		if a.Skip {
			continue
		}
		allServerIDs = append(allServerIDs, a.CanonicalID)
		allServerIDs = append(allServerIDs, a.DuplicateIDs...)
	}

	if *backupTo != "" {
		fmt.Printf("\nWriting backup to %s …\n", *backupTo)
		if err := writeBackup(ctx, db, allServerIDs, *backupTo); err != nil {
			log.Fatalf("backup failed: %v — aborting, no changes made", err)
		}
		fmt.Printf("Backup written ✓\n")
	}

	fmt.Println("\nApplying…")
	consolidated := 0
	skipped := 0
	for _, a := range actions {
		if a.Skip {
			skipped++
			continue
		}
		if err := applyConsolidation(ctx, db, a); err != nil {
			log.Fatalf("apply %q: %v", a.Name, err)
		}
		fmt.Printf("  ✓  %-30s  canonical=%s  removed=%d dup(s)\n",
			a.Name, a.CanonicalID, len(a.DuplicateIDs))
		consolidated++
	}
	fmt.Printf("\nDone. Consolidated=%d, Skipped=%d\n", consolidated, skipped)
}

// --------------------------------------------------------------------------
// Analysis — all DB work done in a handful of batch queries, not per-row loops
// --------------------------------------------------------------------------

// analyzeAll loads all duplicate groups and returns one groupAction per name.
// Total DB round-trips: 6 (findDupNames + loadAllRows + checkGlobals + 4×batchCountGrants),
// regardless of how many duplicate rows or tenants exist.
func analyzeAll(ctx context.Context, db *sql.DB, encKey string) ([]groupAction, error) {
	// 1. Find names with more than one tenant-scoped row.
	dupNames, err := findDupNames(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("find dup names: %w", err)
	}
	if len(dupNames) == 0 {
		return nil, nil
	}
	fmt.Printf("Found %d name(s) with duplicates: %v\n\n", len(dupNames), dupNames)

	// 2. Load all tenant-scoped rows for all dup names in one query.
	allRows, err := loadAllGroupRows(ctx, db, dupNames)
	if err != nil {
		return nil, fmt.Errorf("load rows: %w", err)
	}

	// 3. Check which names already have a global row (one query for all names).
	hasGlobal, err := checkExistingGlobals(ctx, db, dupNames)
	if err != nil {
		return nil, fmt.Errorf("check globals: %w", err)
	}

	// 4. Group rows by name and decide action per group.
	byName := make(map[string][]serverRow, len(dupNames))
	for _, r := range allRows {
		byName[r.Name] = append(byName[r.Name], r)
	}

	actions := make([]groupAction, 0, len(dupNames))
	var allDupIDs []string // collect for batch grant count

	for _, name := range dupNames {
		a := groupAction{Name: name}

		if globalID, ok := hasGlobal[name]; ok {
			a.Skip = true
			a.SkipReason = fmt.Sprintf("global row already exists (id=%s)", globalID)
			actions = append(actions, a)
			continue
		}

		srvRows := byName[name]
		if len(srvRows) == 0 {
			a.Skip = true
			a.SkipReason = "no tenant-scoped rows found (race?)"
			actions = append(actions, a)
			continue
		}

		configs := make([]serverConfig, len(srvRows))
		for i, r := range srvRows {
			cfg, err := extractConfig(r, encKey)
			if err != nil {
				return nil, fmt.Errorf("row %s: %w", r.ID, err)
			}
			configs[i] = cfg
		}

		if !allEqual(configs) {
			a.Skip = true
			a.SkipReason = "config fields differ across tenants — manual review required"
			actions = append(actions, a)
			continue
		}

		sort.Slice(srvRows, func(i, j int) bool {
			return srvRows[i].CreatedAt.Before(srvRows[j].CreatedAt)
		})
		a.CanonicalID = srvRows[0].ID
		for _, r := range srvRows[1:] {
			a.DuplicateIDs = append(a.DuplicateIDs, r.ID)
			allDupIDs = append(allDupIDs, r.ID)
		}

		actions = append(actions, a)
	}

	// 5. Batch-count grants for all dup IDs across all groups (4 queries total).
	if len(allDupIDs) > 0 {
		counts, err := batchCountGrants(ctx, db, allDupIDs)
		if err != nil {
			return nil, fmt.Errorf("count grants: %w", err)
		}
		for i := range actions {
			if actions[i].Skip || len(actions[i].DuplicateIDs) == 0 {
				continue
			}
			actions[i].GrantCounts = make(map[string]grantCount, len(actions[i].DuplicateIDs))
			for _, id := range actions[i].DuplicateIDs {
				actions[i].GrantCounts[id] = counts[id]
			}
		}
	}

	return actions, nil
}

func findDupNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name
		 FROM mcp_servers
		 WHERE tenant_id IS NOT NULL
		 GROUP BY name
		 HAVING COUNT(*) > 1
		 ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// loadAllGroupRows fetches all tenant-scoped rows for every name in one query.
func loadAllGroupRows(ctx context.Context, db *sql.DB, names []string) ([]serverRow, error) {
	args := make([]any, len(names))
	placeholders := make([]byte, 0, len(names)*4)
	for i, n := range names {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
		args[i] = n
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id, name,
		        COALESCE(display_name,''), transport,
		        COALESCE(command,''), COALESCE(args,'[]'::jsonb),
		        COALESCE(url,''), COALESCE(headers,'{}'::jsonb),
		        COALESCE(env,'{}'::jsonb), COALESCE(api_key,''),
		        COALESCE(tool_prefix,''), timeout_sec,
		        COALESCE(settings,'{}'::jsonb), enabled,
		        created_by, created_at, tenant_id
		 FROM mcp_servers
		 WHERE name IN (`+string(placeholders)+`) AND tenant_id IS NOT NULL
		 ORDER BY name, created_at`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []serverRow
	for rows.Next() {
		var r serverRow
		if err := rows.Scan(
			&r.ID, &r.Name, &r.DisplayName, &r.Transport,
			&r.Command, &r.Args,
			&r.URL, &r.Headers,
			&r.Env, &r.APIKey,
			&r.ToolPrefix, &r.TimeoutSec,
			&r.Settings, &r.Enabled,
			&r.CreatedBy, &r.CreatedAt, &r.TenantID,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// checkExistingGlobals returns a map[name]→id for names that already have a global row.
func checkExistingGlobals(ctx context.Context, db *sql.DB, names []string) (map[string]string, error) {
	args := make([]any, len(names))
	placeholders := make([]byte, 0, len(names)*4)
	for i, n := range names {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
		args[i] = n
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, name FROM mcp_servers WHERE name IN (`+string(placeholders)+`) AND tenant_id IS NULL`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result[name] = id
	}
	return result, rows.Err()
}

// batchCountGrants returns grant counts for all given server IDs using one query
// per grant table (4 queries total for the whole batch).
func batchCountGrants(ctx context.Context, db *sql.DB, serverIDs []string) (map[string]grantCount, error) {
	args := make([]any, len(serverIDs))
	placeholders := make([]byte, 0, len(serverIDs)*4)
	for i, id := range serverIDs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
		args[i] = id
	}
	ph := string(placeholders)

	result := make(map[string]grantCount, len(serverIDs))
	for _, id := range serverIDs {
		result[id] = grantCount{}
	}

	type tableQuery struct {
		table  string
		setter func(gc *grantCount, n int)
	}
	tables := []tableQuery{
		{"mcp_agent_grants", func(gc *grantCount, n int) { gc.AgentGrants = n }},
		{"mcp_user_grants", func(gc *grantCount, n int) { gc.UserGrants = n }},
		{"mcp_user_credentials", func(gc *grantCount, n int) { gc.UserCredentials = n }},
		{"mcp_access_requests", func(gc *grantCount, n int) { gc.AccessRequests = n }},
	}

	for _, tq := range tables {
		rows, err := db.QueryContext(ctx,
			fmt.Sprintf("SELECT server_id::text, COUNT(*) FROM %s WHERE server_id IN (%s) GROUP BY server_id", tq.table, ph),
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tq.table, err)
		}
		for rows.Next() {
			var sid string
			var cnt int
			if err := rows.Scan(&sid, &cnt); err != nil {
				rows.Close()
				return nil, err
			}
			gc := result[sid]
			tq.setter(&gc, cnt)
			result[sid] = gc
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("%s rows: %w", tq.table, err)
		}
	}

	return result, nil
}

// extractConfig decrypts sensitive fields and unmarshals JSON blobs for
// structural comparison.
func extractConfig(r serverRow, encKey string) (serverConfig, error) {
	cfg := serverConfig{
		Transport:  r.Transport,
		Command:    r.Command,
		URL:        r.URL,
		ToolPrefix: r.ToolPrefix,
		TimeoutSec: r.TimeoutSec,
		Enabled:    r.Enabled,
		APIKey:     r.APIKey,
	}

	// Decrypt api_key if encrypted.
	if encKey != "" && r.APIKey != "" && len(r.APIKey) > 7 && r.APIKey[:7] == "aes-gcm" {
		dec, err := crypto.Decrypt(r.APIKey, encKey)
		if err != nil {
			return cfg, fmt.Errorf("decrypt api_key: %w", err)
		}
		cfg.APIKey = dec
	}

	// Decrypt JSONB blobs: stored as a JSON string ("aes-gcm:…") if encrypted.
	headersRaw, err := decryptJSONBField(r.Headers, encKey)
	if err != nil {
		return cfg, fmt.Errorf("decrypt headers: %w", err)
	}
	envRaw, err := decryptJSONBField(r.Env, encKey)
	if err != nil {
		return cfg, fmt.Errorf("decrypt env: %w", err)
	}

	// Unmarshal JSON fields for structural (key-order-independent) comparison.
	cfg.Args = unmarshalAny(r.Args)
	cfg.Headers = unmarshalAny(headersRaw)
	cfg.Env = unmarshalAny(envRaw)
	cfg.Settings = unmarshalAny(r.Settings)

	return cfg, nil
}

// decryptJSONBField handles the JSONB encryption pattern used in PGMCPServerStore.
// Encrypted blobs are stored as a JSON string literal containing "aes-gcm:…".
// Unencrypted blobs are JSON objects / arrays.
func decryptJSONBField(data []byte, encKey string) ([]byte, error) {
	if encKey == "" || len(data) == 0 || data[0] != '"' {
		return data, nil // not a JSON string → already plaintext JSONB
	}
	var encStr string
	if err := json.Unmarshal(data, &encStr); err != nil {
		return data, nil // unmarshal failed → treat as plaintext
	}
	dec, err := crypto.Decrypt(encStr, encKey)
	if err != nil {
		return nil, err
	}
	return []byte(dec), nil
}

func unmarshalAny(data []byte) any {
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		// Fallback: compare as canonical JSON-encoded bytes.
		return string(bytes.TrimSpace(data))
	}
	return v
}

func allEqual(cfgs []serverConfig) bool {
	if len(cfgs) <= 1 {
		return true
	}
	for i := 1; i < len(cfgs); i++ {
		if !reflect.DeepEqual(cfgs[0], cfgs[i]) {
			return false
		}
	}
	return true
}

// --------------------------------------------------------------------------
// Plan output
// --------------------------------------------------------------------------

func printPlan(actions []groupAction) {
	fmt.Println("=== CONSOLIDATION PLAN ===")
	consolidate := 0
	skip := 0
	for _, a := range actions {
		if a.Skip {
			skip++
			fmt.Printf("  SKIP  %-30s  — %s\n", a.Name, a.SkipReason)
		} else {
			consolidate++
			totalGrants := 0
			for _, gc := range a.GrantCounts {
				totalGrants += gc.AgentGrants + gc.UserGrants + gc.UserCredentials + gc.AccessRequests
			}
			fmt.Printf("  MERGE %-30s  canonical=%s  +%d dup(s)  grants_to_rehome=%d\n",
				a.Name, a.CanonicalID, len(a.DuplicateIDs), totalGrants)
			fmt.Printf("        duplicates: %v\n", a.DuplicateIDs)
		}
	}
	fmt.Printf("\nTotal: %d to consolidate, %d to skip\n", consolidate, skip)
}

// --------------------------------------------------------------------------
// Backup
// --------------------------------------------------------------------------

// writeBackup dumps all rows (from mcp_servers and related grant tables) that
// are involved in the consolidation — both canonical and duplicate rows — into a
// JSON file. The file is written atomically (temp file → rename).
func writeBackup(ctx context.Context, db *sql.DB, serverIDs []string, path string) error {
	if len(serverIDs) == 0 {
		return fmt.Errorf("no server IDs to back up")
	}

	bundle := backupBundle{CapturedAt: time.Now().UTC()}
	var err error

	bundle.MCPServers, err = dumpTable(ctx, db, "mcp_servers", "id", serverIDs)
	if err != nil {
		return fmt.Errorf("dump mcp_servers: %w", err)
	}
	bundle.AgentGrants, err = dumpTable(ctx, db, "mcp_agent_grants", "server_id", serverIDs)
	if err != nil {
		return fmt.Errorf("dump mcp_agent_grants: %w", err)
	}
	bundle.UserGrants, err = dumpTable(ctx, db, "mcp_user_grants", "server_id", serverIDs)
	if err != nil {
		return fmt.Errorf("dump mcp_user_grants: %w", err)
	}
	bundle.UserCredentials, err = dumpTable(ctx, db, "mcp_user_credentials", "server_id", serverIDs)
	if err != nil {
		return fmt.Errorf("dump mcp_user_credentials: %w", err)
	}
	bundle.AccessRequests, err = dumpTable(ctx, db, "mcp_access_requests", "server_id", serverIDs)
	if err != nil {
		return fmt.Errorf("dump mcp_access_requests: %w", err)
	}

	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Write via temp file → rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	fmt.Printf("  mcp_servers=%d  agent_grants=%d  user_grants=%d  user_credentials=%d  access_requests=%d\n",
		len(bundle.MCPServers), len(bundle.AgentGrants), len(bundle.UserGrants),
		len(bundle.UserCredentials), len(bundle.AccessRequests))

	return nil
}

// dumpTable fetches all rows from table where column IN (ids) and returns them
// as a slice of generic maps (column → value). JSONB columns are kept as
// json.RawMessage so the backup is human-readable.
func dumpTable(ctx context.Context, db *sql.DB, table, column string, ids []string) ([]map[string]any, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Build $1,$2,… placeholder list.
	placeholders := make([]byte, 0, len(ids)*4)
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
		args[i] = id
	}

	q := fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s)", table, column, placeholders)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var result []map[string]any
	for rows.Next() {
		// Scan into []any; PG driver returns []byte for JSONB/BYTEA.
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := vals[i]
			// Keep []byte as string so JSON marshal doesn't base64-encode it.
			if b, ok := v.([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = v
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// --------------------------------------------------------------------------
// Apply
// --------------------------------------------------------------------------

func applyConsolidation(ctx context.Context, db *sql.DB, a groupAction) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error is intentional

	canonicalUUID, err := uuid.Parse(a.CanonicalID)
	if err != nil {
		return fmt.Errorf("parse canonical uuid: %w", err)
	}

	// Step 1: promote canonical row to global scope.
	if _, err := tx.ExecContext(ctx,
		"UPDATE mcp_servers SET tenant_id = NULL, updated_at = NOW() WHERE id = $1",
		canonicalUUID,
	); err != nil {
		return fmt.Errorf("promote canonical: %w", err)
	}

	// Step 2: for each duplicate, re-home its grants then delete the row.
	for _, dupIDStr := range a.DuplicateIDs {
		dupID, err := uuid.Parse(dupIDStr)
		if err != nil {
			return fmt.Errorf("parse dup uuid: %w", err)
		}

		if err := rehomeGrants(ctx, tx, dupID, canonicalUUID); err != nil {
			return fmt.Errorf("rehome grants from %s: %w", dupID, err)
		}

		// Delete the dup row; ON DELETE CASCADE removes any remaining grants
		// (those that would have conflicted with canonical's existing grants).
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM mcp_servers WHERE id = $1", dupID,
		); err != nil {
			return fmt.Errorf("delete dup %s: %w", dupID, err)
		}
	}

	return tx.Commit()
}

// rehomeGrants copies grants from dupID to canonicalID using INSERT … ON CONFLICT DO NOTHING,
// so that canonical's existing grants are preserved and only new grants are added.
func rehomeGrants(ctx context.Context, tx *sql.Tx, dupID, canonicalID uuid.UUID) error {
	// mcp_agent_grants — UNIQUE(server_id, agent_id)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_agent_grants
		  (id, server_id, agent_id, enabled, tool_allow, tool_deny, config_overrides, granted_by, created_at, tenant_id)
		SELECT $1, $2, agent_id, enabled, tool_allow, tool_deny, config_overrides, granted_by, created_at, tenant_id
		FROM mcp_agent_grants
		WHERE server_id = $3
		ON CONFLICT (server_id, agent_id) DO NOTHING`,
		uuid.Must(uuid.NewV7()), canonicalID, dupID,
	); err != nil {
		return fmt.Errorf("rehome agent_grants: %w", err)
	}

	// mcp_user_grants — UNIQUE(server_id, user_id)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_user_grants
		  (id, server_id, user_id, enabled, tool_allow, tool_deny, granted_by, created_at, tenant_id)
		SELECT $1, $2, user_id, enabled, tool_allow, tool_deny, granted_by, created_at, tenant_id
		FROM mcp_user_grants
		WHERE server_id = $3
		ON CONFLICT (server_id, user_id) DO NOTHING`,
		uuid.Must(uuid.NewV7()), canonicalID, dupID,
	); err != nil {
		return fmt.Errorf("rehome user_grants: %w", err)
	}

	// mcp_user_credentials — UNIQUE(server_id, user_id, tenant_id)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_user_credentials
		  (id, server_id, user_id, api_key, headers, env, tenant_id, created_at, updated_at)
		SELECT gen_random_uuid(), $1, user_id, api_key, headers, env, tenant_id, created_at, updated_at
		FROM mcp_user_credentials
		WHERE server_id = $2
		ON CONFLICT (server_id, user_id, tenant_id) DO NOTHING`,
		canonicalID, dupID,
	); err != nil {
		return fmt.Errorf("rehome user_credentials: %w", err)
	}

	// mcp_access_requests — no UNIQUE constraint, just update server_id.
	if _, err := tx.ExecContext(ctx,
		"UPDATE mcp_access_requests SET server_id = $1 WHERE server_id = $2",
		canonicalID, dupID,
	); err != nil {
		return fmt.Errorf("rehome access_requests: %w", err)
	}

	return nil
}
