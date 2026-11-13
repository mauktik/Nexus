// Command sqliteprobe is the WI 0.1 check-5 probe (A-14, A-39). It opens
// Pi-hole's gravity.db read-only with the exact DSN WI 2.1 will use, prints
// the journal mode, the info rows and the domainlist row count, and exits.
// spike.sh runs it under every sandboxing directive of deploy/nexusd.service.
//
// It is a nested module so the root go.mod and ./... stay untouched.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"runtime/debug"

	_ "modernc.org/sqlite"
)

const sqliteModule = "modernc.org/sqlite"

// dsn is WI 2.1's DSN verbatim. No immutable=1: it disables locking and
// change detection and would mask the failure this probe exists to find.
func dsn(path string) string {
	return "file:" + path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
}

func main() {
	path := "/etc/pihole/gravity.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if err := run(path); err != nil {
		fmt.Fprintf(os.Stderr, "sqliteprobe: %v\n", err)
		os.Exit(1)
	}
}

func run(path string) error {
	fmt.Printf("module %s %s\n", sqliteModule, moduleVersion())
	fmt.Printf("dsn %s\n", dsn(path))

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return fmt.Errorf("open %s: %v", path, err)
	}
	defer db.Close()
	// One connection: the per-connection pragmas and the reads share it.
	db.SetMaxOpenConns(1)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("pragma journal_mode: %v", err)
	}
	fmt.Printf("journal_mode %s\n", mode)

	var queryOnly int
	if err := db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		return fmt.Errorf("pragma query_only: %v", err)
	}
	fmt.Printf("query_only %d\n", queryOnly)

	if err := printInfo(db); err != nil {
		return err
	}

	var n int64
	if err := db.QueryRow("SELECT count(*) FROM domainlist").Scan(&n); err != nil {
		return fmt.Errorf("count domainlist: %v", err)
	}
	fmt.Printf("domainlist_count %d\n", n)
	return nil
}

func printInfo(db *sql.DB) error {
	rows, err := db.Query("SELECT property, value FROM info ORDER BY property")
	if err != nil {
		return fmt.Errorf("query info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var property string
		var value sql.NullString
		if err := rows.Scan(&property, &value); err != nil {
			return fmt.Errorf("scan info: %v", err)
		}
		fmt.Printf("info %s=%s\n", property, value.String)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read info: %v", err)
	}
	return nil
}

func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range bi.Deps {
		if d.Path == sqliteModule {
			if d.Replace != nil {
				return d.Version + " => " + d.Replace.Version
			}
			return d.Version
		}
	}
	return "unknown"
}
