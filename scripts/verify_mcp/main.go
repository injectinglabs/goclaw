// scripts/verify_mcp/main.go — quick sanity-check for post-dedupe state
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	dsn := os.Getenv("GOCLAW_POSTGRES_DSN")
	if dsn == "" {
		log.Fatal("GOCLAW_POSTGRES_DSN required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	names := []string{"calendar-mcp", "connectors", "docs-mcp", "document-mcp", "drive-mcp", "gmail-mcp", "sheets-mcp", "slack-mcp"}

	fmt.Println("=== mcp_servers per name ===")
	fmt.Printf("%-20s  %-8s  %s\n", "name", "scope", "count")
	fmt.Println("--------------------------------------------")
	for _, n := range names {
		var globalCnt, tenantCnt int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mcp_servers WHERE name=$1 AND tenant_id IS NULL", n).Scan(&globalCnt)
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mcp_servers WHERE name=$1 AND tenant_id IS NOT NULL", n).Scan(&tenantCnt)
		globalMark := ""
		if globalCnt != 1 {
			globalMark = " ← WRONG"
		}
		tenantMark := ""
		if tenantCnt != 0 {
			tenantMark = " ← WRONG (duplicates remain!)"
		}
		fmt.Printf("%-20s  global=%d%s  tenant=%d%s\n", n, globalCnt, globalMark, tenantCnt, tenantMark)
	}

	fmt.Println("\n=== agent_grants per global server ===")
	fmt.Printf("%-20s  %s\n", "name", "grant_count")
	fmt.Println("-----------------------------------")
	rows, err := db.QueryContext(ctx, `
		SELECT s.name, COUNT(g.id)
		FROM mcp_servers s
		LEFT JOIN mcp_agent_grants g ON g.server_id = s.id
		WHERE s.name IN ('calendar-mcp','connectors','docs-mcp','document-mcp','drive-mcp','gmail-mcp','sheets-mcp','slack-mcp')
		  AND s.tenant_id IS NULL
		GROUP BY s.name ORDER BY s.name`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var cnt int
		rows.Scan(&name, &cnt)
		fmt.Printf("%-20s  %d\n", name, cnt)
	}

	fmt.Println("\n=== orphaned grants (server_id points to deleted row) ===")
	var orphans int
	db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mcp_agent_grants g
		WHERE NOT EXISTS (SELECT 1 FROM mcp_servers s WHERE s.id = g.server_id)`).Scan(&orphans)
	if orphans == 0 {
		fmt.Println("  none ✓")
	} else {
		fmt.Printf("  %d orphaned grants ← PROBLEM\n", orphans)
	}

	fmt.Println("\n=== total mcp_servers rows ===")
	var total, globalTotal, tenantTotal int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mcp_servers").Scan(&total)
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mcp_servers WHERE tenant_id IS NULL").Scan(&globalTotal)
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mcp_servers WHERE tenant_id IS NOT NULL").Scan(&tenantTotal)
	fmt.Printf("  total=%d  global=%d  tenant-scoped=%d\n", total, globalTotal, tenantTotal)
}
