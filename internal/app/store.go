package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Store struct {
	dir     string
	db      *sql.DB
	backend string
	dsn     string
}

func NewStore(dir string) *Store {
	s, err := openStore(dir, "sqlite", "")
	if err != nil {
		panic(err)
	}
	return s
}

func OpenStore(dir string) (*Store, error) {
	backend, databaseURL, err := resolveStoreConfig(os.Getenv("STORAGE_BACKEND"), os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	return openStore(dir, backend, databaseURL)
}

func resolveStoreConfig(configuredBackend, configuredURL string) (string, string, error) {
	backend := strings.ToLower(strings.TrimSpace(configuredBackend))
	databaseURL := strings.TrimSpace(configuredURL)
	if backend != "" && backend != "sqlite" && backend != "postgres" {
		return "", "", fmt.Errorf("unsupported STORAGE_BACKEND %q (use sqlite or postgres)", backend)
	}
	if databaseURL != "" {
		switch {
		case strings.HasPrefix(databaseURL, "sqlite://"):
			return "sqlite", databaseURL, nil
		case strings.HasPrefix(databaseURL, "postgres://"), strings.HasPrefix(databaseURL, "postgresql://"):
			return "postgres", databaseURL, nil
		default:
			return "", "", errors.New("DATABASE_URL must start with sqlite://, postgres://, or postgresql://")
		}
	}
	if backend == "postgres" {
		return "", "", errors.New("PostgreSQL requires DATABASE_URL=postgresql://...")
	}
	return "sqlite", "", nil
}

func openStore(dir, backend, databaseURL string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, backend: backend}
	driver := "sqlite"
	switch backend {
	case "sqlite":
		s.dsn = filepath.Join(dir, "chatgpt2api.db")
		if databaseURL != "" {
			if !strings.HasPrefix(databaseURL, "sqlite://") {
				return nil, errors.New("SQLite DATABASE_URL must start with sqlite://")
			}
			s.dsn = strings.TrimPrefix(databaseURL, "sqlite://")
			if !filepath.IsAbs(s.dsn) {
				return nil, errors.New("SQLite DATABASE_URL requires an absolute path")
			}
		}
		if err := os.MkdirAll(filepath.Dir(s.dsn), 0700); err != nil {
			return nil, err
		}
	case "postgres":
		if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
			return nil, errors.New("PostgreSQL requires DATABASE_URL=postgresql://...")
		}
		driver, s.dsn = "pgx", databaseURL
	default:
		return nil, fmt.Errorf("unsupported STORAGE_BACKEND %q (use sqlite or postgres)", backend)
	}
	var err error
	connectDSN := s.dsn
	if backend == "sqlite" {
		connectDSN = (&url.URL{Scheme: "file", Path: s.dsn, RawQuery: "_txlock=immediate&_pragma=busy_timeout(5000)"}).String()
	}
	s.db, err = sql.Open(driver, connectDSN)
	if err != nil {
		return nil, err
	}
	if backend == "sqlite" {
		s.db.SetMaxOpenConns(1)
	} else {
		s.db.SetMaxOpenConns(10)
	}
	if err = s.db.Ping(); err == nil && backend == "sqlite" {
		_, err = s.db.Exec("PRAGMA busy_timeout = 5000")
		if err == nil {
			_, err = s.db.Exec("PRAGMA journal_mode = WAL")
		}
		if err == nil {
			err = os.Chmod(s.dsn, 0600)
		}
	}
	if err == nil {
		err = s.initSchema()
	}
	if err == nil {
		err = s.importLegacy()
	}
	if err == nil {
		err = s.migrateAccountRows()
	}
	if err == nil {
		err = s.migrateLogColumns()
	}
	if err == nil {
		err = s.mergeLegacyCallLogs()
	}
	if err == nil {
		err = s.migrateImageTaskBase64()
	}
	if err != nil {
		s.db.Close()
		return nil, fmt.Errorf("open %s storage: %w", backend, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) bind(n int) string {
	if s.backend == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func (s *Store) initSchema() error {
	statements := []string{
		"CREATE TABLE IF NOT EXISTS app_documents (name TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS app_migrations (name TEXT PRIMARY KEY)",
		"CREATE TABLE IF NOT EXISTS app_accounts (token_hash TEXT PRIMARY KEY, ordinal BIGINT NOT NULL, payload TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS app_auth_keys (key_id TEXT PRIMARY KEY, ordinal BIGINT NOT NULL, payload TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS app_collection_locks (name TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 0)",
		"CREATE TABLE IF NOT EXISTS app_account_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL, proxy TEXT NOT NULL DEFAULT '', enabled BOOLEAN NOT NULL DEFAULT TRUE, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS app_proxy_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL, strategy TEXT NOT NULL, rotation_interval_minutes REAL NOT NULL DEFAULT 0, enabled BOOLEAN NOT NULL DEFAULT TRUE, notes TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS app_proxy_nodes (id TEXT PRIMARY KEY, group_id TEXT NOT NULL REFERENCES app_proxy_groups(id) ON DELETE CASCADE, name TEXT NOT NULL, url TEXT NOT NULL, enabled BOOLEAN NOT NULL DEFAULT TRUE, image_concurrency_limit INTEGER NOT NULL DEFAULT 0, notes TEXT NOT NULL DEFAULT '', health_state TEXT NOT NULL DEFAULT 'unknown', checked_at TEXT NOT NULL DEFAULT '', latency_ms BIGINT, health_error TEXT NOT NULL DEFAULT '', success_count BIGINT NOT NULL DEFAULT 0, fail_count BIGINT NOT NULL DEFAULT 0, ordinal BIGINT NOT NULL DEFAULT 0)",
		"CREATE TABLE IF NOT EXISTS app_proxy_settings (id INTEGER PRIMARY KEY, default_mode TEXT NOT NULL DEFAULT 'custom', default_group_id TEXT NOT NULL DEFAULT '', default_url TEXT NOT NULL DEFAULT '', fallback_mode TEXT NOT NULL DEFAULT '', fallback_group_id TEXT NOT NULL DEFAULT '', fallback_url TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL)",
	}
	if s.backend == "postgres" {
		statements = append(statements, "CREATE TABLE IF NOT EXISTS app_logs (seq BIGSERIAL PRIMARY KEY, id TEXT NOT NULL, payload TEXT NOT NULL)")
	} else {
		statements = append(statements, "CREATE TABLE IF NOT EXISTS app_logs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL, payload TEXT NOT NULL)")
	}
	statements = append(statements, "CREATE INDEX IF NOT EXISTS app_logs_id_idx ON app_logs (id)")
	statements = append(statements, "CREATE INDEX IF NOT EXISTS app_proxy_nodes_group_idx ON app_proxy_nodes (group_id, ordinal)")
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	for _, name := range []string{"accounts", "auth_keys", "proxy_references"} {
		if _, err := s.db.Exec("INSERT INTO app_collection_locks (name, version) VALUES ("+s.bind(1)+", 0) ON CONFLICT (name) DO NOTHING", name); err != nil {
			return err
		}
	}
	return nil
}

var legacyDocuments = []string{"accounts.json", "auth_keys.json", "gallery.json", "image_tasks.json", "image_owners.json", "image_prompts.json", "image_tags.json", "chat_conversations.json", "cpa_pools.json", "sub2api_servers.json", "register.json", "ddg_aliases.json", "outlook_token_used.json"}

func (s *Store) importLegacy() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.backend == "postgres" {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('chatgpt2api_legacy_json_v1'))"); err != nil {
			return err
		}
	}
	var marker string
	err = tx.QueryRow("SELECT name FROM app_migrations WHERE name = 'legacy_json_v1'").Scan(&marker)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, name := range legacyDocuments {
		data, err := os.ReadFile(s.path(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read legacy %s: %w", name, err)
		}
		if !json.Valid(data) {
			return fmt.Errorf("legacy %s contains invalid JSON", name)
		}
		stmt := "INSERT INTO app_documents (name, payload) VALUES (" + s.bind(1) + ", " + s.bind(2) + ") ON CONFLICT (name) DO NOTHING"
		if _, err := tx.Exec(stmt, name, string(data)); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}
	}
	if data, err := os.ReadFile(s.path("logs.jsonl")); err == nil {
		for index, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var item map[string]any
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				return fmt.Errorf("legacy logs.jsonl line %d: %w", index+1, err)
			}
			id := strAny(item["id"], "")
			if id == "" {
				id = randID(8)
				item["id"] = id
				encoded, _ := json.Marshal(item)
				line = string(encoded)
			}
			stmt := "INSERT INTO app_logs (id, payload) VALUES (" + s.bind(1) + ", " + s.bind(2) + ")"
			if _, err := tx.Exec(stmt, id, line); err != nil {
				return fmt.Errorf("import logs.jsonl line %d: %w", index+1, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read legacy logs.jsonl: %w", err)
	}
	if _, err := tx.Exec("INSERT INTO app_migrations (name) VALUES ('legacy_json_v1') ON CONFLICT (name) DO NOTHING"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadDocument(name string, value any) error {
	payload, err := s.documentPayload(name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(payload), value)
}

func (s *Store) documentPayload(name string) (string, error) {
	var payload string
	err := s.db.QueryRow("SELECT payload FROM app_documents WHERE name = "+s.bind(1), name).Scan(&payload)
	return payload, err
}

func (s *Store) SaveDocument(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	stmt := "INSERT INTO app_documents (name, payload) VALUES (" + s.bind(1) + ", " + s.bind(2) + ") ON CONFLICT (name) DO UPDATE SET payload = EXCLUDED.payload"
	_, err = s.db.Exec(stmt, name, string(data))
	return err
}

func (s *Store) replaceDocumentIfPayload(name, previous string, value any) (bool, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	stmt := "UPDATE app_documents SET payload = " + s.bind(1) + " WHERE name = " + s.bind(2) + " AND payload = " + s.bind(3)
	result, err := s.db.Exec(stmt, string(data), name, previous)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func updateDocument[T any](s *Store, name string, fallback T, fn func(T) T) error {
	data, err := json.Marshal(fallback)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert := "INSERT INTO app_documents (name, payload) VALUES (" + s.bind(1) + ", " + s.bind(2) + ") ON CONFLICT (name) DO NOTHING"
	if _, err := tx.Exec(insert, name, string(data)); err != nil {
		return err
	}
	query := "SELECT payload FROM app_documents WHERE name = " + s.bind(1)
	if s.backend == "postgres" {
		query += " FOR UPDATE"
	}
	var payload string
	if err := tx.QueryRow(query, name).Scan(&payload); err != nil {
		return err
	}
	var value T
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	data, err = json.Marshal(fn(value))
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE app_documents SET payload = "+s.bind(1)+" WHERE name = "+s.bind(2), string(data), name); err != nil {
		return err
	}
	return tx.Commit()
}

func loadDocument[T any](s *Store, name string, fallback T) T {
	value := fallback
	if err := s.LoadDocument(name, &value); err != nil {
		log.Printf("storage read %s: %v", name, err)
	}
	return value
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

func readJSONFile[T any](path string, fallback T) T {
	b, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return fallback
	}
	return out
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) LoadImageIndex() map[string]imageStorageEntry {
	items := readJSONFile(s.path("image_index.json"), map[string]imageStorageEntry{})
	if items == nil {
		return map[string]imageStorageEntry{}
	}
	return items
}

func (s *Store) SaveImageIndex(items map[string]imageStorageEntry) error {
	return writeJSONFile(s.path("image_index.json"), items)
}

func (s *Store) LoadAccounts() []Account {
	items, err := loadAccountRows(s)
	if err != nil {
		log.Printf("storage read accounts: %v", err)
		return []Account{}
	}
	out := make([]Account, 0, len(items))
	for _, a := range items {
		if a.AccessToken == "" {
			continue
		}
		a.IsSignupToken = isSignupAccessToken(a.AccessToken)
		if a.Type == "" {
			a.Type = "free"
		}
		if a.Status == "" {
			a.Status = "正常"
		}
		if a.SourceType == "" {
			a.SourceType = "web"
		}
		if a.InitialQuota < a.Quota {
			a.InitialQuota = a.Quota
		}
		out = append(out, a)
	}
	return out
}

func (s *Store) SaveAccounts(items []Account) error {
	return replaceAccountRows(s, func([]Account) []Account { return items })
}

func (s *Store) UpdateAccounts(fn func([]Account) []Account) error {
	return replaceAccountRows(s, fn)
}

type authKeysWrap struct {
	Items []UserKey `json:"items"`
}

func (s *Store) LoadAuthKeys() []UserKey {
	items, err := s.LoadAuthKeysWithError()
	if err != nil {
		log.Printf("storage read auth_keys.json: %v", err)
	}
	return items
}

func (s *Store) LoadAuthKeysWithError() ([]UserKey, error) {
	items, err := loadAuthKeyRows(s)
	if err != nil {
		return nil, err
	}
	return normalizeKeys(items), nil
}

func (s *Store) SaveAuthKeys(items []UserKey) error {
	return replaceAuthKeyRows(s, func([]UserKey) []UserKey { return items })
}

func (s *Store) UpdateAuthKeys(fn func([]UserKey) []UserKey) error {
	return replaceAuthKeyRows(s, func(items []UserKey) []UserKey { return fn(normalizeKeys(items)) })
}

func (s *Store) LoadGallery() []GalleryItem {
	return loadDocument(s, "gallery.json", []GalleryItem{})
}
func (s *Store) SaveGallery(items []GalleryItem) error {
	return s.SaveDocument("gallery.json", items)
}
func (s *Store) UpdateGallery(fn func([]GalleryItem) []GalleryItem) error {
	return updateDocument(s, "gallery.json", []GalleryItem{}, fn)
}
func (s *Store) LoadTasks() []ImageTask {
	return loadDocument(s, "image_tasks.json", []ImageTask{})
}
func (s *Store) SaveTasks(items []ImageTask) error {
	return s.SaveDocument("image_tasks.json", items)
}
func (s *Store) UpdateTasks(fn func([]ImageTask) []ImageTask) error {
	return updateDocument(s, "image_tasks.json", []ImageTask{}, fn)
}
func (s *Store) LoadOwners() map[string]string {
	return loadDocument(s, "image_owners.json", map[string]string{})
}
func (s *Store) SaveOwners(items map[string]string) error {
	return s.SaveDocument("image_owners.json", items)
}
func (s *Store) UpdateOwners(fn func(map[string]string) map[string]string) error {
	return updateDocument(s, "image_owners.json", map[string]string{}, fn)
}
func (s *Store) LoadPrompts() map[string]map[string]any {
	return loadDocument(s, "image_prompts.json", map[string]map[string]any{})
}
func (s *Store) SavePrompts(items map[string]map[string]any) error {
	return s.SaveDocument("image_prompts.json", items)
}
func (s *Store) UpdatePrompts(fn func(map[string]map[string]any) map[string]map[string]any) error {
	return updateDocument(s, "image_prompts.json", map[string]map[string]any{}, fn)
}
func (s *Store) LoadTags() map[string][]string {
	return loadDocument(s, "image_tags.json", map[string][]string{})
}
func (s *Store) SaveTags(items map[string][]string) error {
	return s.SaveDocument("image_tags.json", items)
}
func (s *Store) LoadList(name string) []map[string]any {
	return loadDocument(s, name, []map[string]any{})
}
func (s *Store) SaveList(name string, v []map[string]any) error {
	return s.SaveDocument(name, v)
}
func (s *Store) UpdateList(name string, fn func([]map[string]any) []map[string]any) error {
	return updateDocument(s, name, []map[string]any{}, fn)
}

func ensureNotDir(path string) error {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New(path + " is a directory")
	}
	return nil
}

func hashKey(key string) string { h := sha256.Sum256([]byte(key)); return hex.EncodeToString(h[:]) }
