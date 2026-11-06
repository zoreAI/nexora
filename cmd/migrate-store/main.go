package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/zoreAI/nexora/internal/app"
	_ "modernc.org/sqlite"
)

type tableSpec struct {
	name string
	key  string
}

var tables = []tableSpec{
	{"app_account_groups", "id"},
	{"app_proxy_groups", "id"},
	{"app_proxy_nodes", "id"},
	{"app_proxy_settings", "id"},
	{"app_collection_locks", "name"},
	{"app_documents", "name"},
	{"app_migrations", "name"},
	{"app_accounts", "token_hash"},
	{"app_auth_keys", "key_id"},
	{"app_logs", "seq"},
}

func main() {
	sourcePath := flag.String("sqlite", "", "path to a consistent SQLite snapshot")
	flag.Parse()
	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "load .env:", err)
		os.Exit(1)
	}
	if err := migrate(*sourcePath, os.Getenv("DATABASE_URL")); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
}

func migrate(sourcePath, destinationURL string) error {
	if sourcePath == "" || destinationURL == "" {
		return errors.New("-sqlite and DATABASE_URL are required")
	}
	if !strings.HasPrefix(destinationURL, "postgres://") && !strings.HasPrefix(destinationURL, "postgresql://") {
		return errors.New("DATABASE_URL must be PostgreSQL")
	}
	source, err := sql.Open("sqlite", "file:"+sourcePath+"?mode=ro")
	if err != nil {
		return err
	}
	defer source.Close()
	if err := source.Ping(); err != nil {
		return fmt.Errorf("open SQLite snapshot: %w", err)
	}
	destination, err := sql.Open("pgx", destinationURL)
	if err != nil {
		return err
	}
	defer destination.Close()
	if err := destination.Ping(); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	var existing int
	if err := destination.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'app_%'").Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return fmt.Errorf("destination contains %d app tables; use an empty database", existing)
	}
	// Initialize the destination with the application's own schema and migrations.
	tempDir, err := os.MkdirTemp("", "nexora-migrate-schema-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	store, err := app.OpenStore(tempDir)
	if err != nil {
		return fmt.Errorf("initialize PostgreSQL schema: %w", err)
	}
	if err := store.Close(); err != nil {
		return err
	}
	tx, err := destination.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := len(tables) - 1; i >= 0; i-- {
		if _, err := tx.Exec("DELETE FROM " + tables[i].name); err != nil {
			return fmt.Errorf("clear initialized %s: %w", tables[i].name, err)
		}
	}
	for _, table := range tables {
		count, err := copyTable(source, tx, table)
		if err != nil {
			return err
		}
		fmt.Printf("copied %s: %d rows\n", table.name, count)
	}
	if _, err := tx.Exec("SELECT setval(pg_get_serial_sequence('app_logs', 'seq'), COALESCE((SELECT MAX(seq) FROM app_logs), 1), EXISTS(SELECT 1 FROM app_logs))"); err != nil {
		return fmt.Errorf("reset log sequence: %w", err)
	}
	for _, table := range tables {
		columns, err := tableColumns(source, table.name)
		if err != nil {
			return err
		}
		fromCount, fromHash, err := digestTable(source, table, columns)
		if err != nil {
			return err
		}
		toCount, toHash, err := digestTable(tx, table, columns)
		if err != nil {
			return err
		}
		if fromCount != toCount || fromHash != toHash {
			return fmt.Errorf("verification failed for %s: source=%d/%x destination=%d/%x", table.name, fromCount, fromHash, toCount, toHash)
		}
		fmt.Printf("verified %s: %d rows\n", table.name, toCount)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Println("migration committed; source SQLite snapshot was not changed")
	return nil
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func tableColumns(db queryer, name string) ([]string, error) {
	rows, err := db.Query("SELECT * FROM " + name + " LIMIT 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.Columns()
}

func copyTable(source *sql.DB, destination *sql.Tx, table tableSpec) (int, error) {
	columns, err := tableColumns(source, table.name)
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	rows, err := source.Query("SELECT " + strings.Join(quoted, ",") + " FROM " + table.name + " ORDER BY " + table.key)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	stmt, err := destination.Prepare("INSERT INTO " + table.name + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	count := 0
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return count, err
		}
		for i, column := range columns {
			values[i] = canonicalValue(column, values[i])
		}
		if _, err := stmt.Exec(values...); err != nil {
			return count, fmt.Errorf("insert %s row %d: %w", table.name, count+1, err)
		}
		count++
	}
	return count, rows.Err()
}

func canonicalValue(column string, value any) any {
	if bytes, ok := value.([]byte); ok {
		value = string(bytes)
	}
	if column == "enabled" {
		if number, ok := value.(int64); ok {
			return number != 0
		}
	}
	return value
}

func digestTable(db queryer, table tableSpec, columns []string) (int, [32]byte, error) {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
	}
	rows, err := db.Query("SELECT " + strings.Join(quoted, ",") + " FROM " + table.name + " ORDER BY " + table.key)
	if err != nil {
		return 0, [32]byte{}, err
	}
	defer rows.Close()
	checksum := sha256.New()
	count := 0
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return count, [32]byte{}, err
		}
		for i, column := range columns {
			values[i] = canonicalValue(column, values[i])
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return count, [32]byte{}, err
		}
		checksum.Write(encoded)
		checksum.Write([]byte{'\n'})
		count++
	}
	if err := rows.Err(); err != nil {
		return count, [32]byte{}, err
	}
	var digest [32]byte
	copy(digest[:], checksum.Sum(nil))
	return count, digest, nil
}
