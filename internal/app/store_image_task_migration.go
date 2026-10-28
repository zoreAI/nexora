package app

import (
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) migrateImageTaskBase64() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.backend == "postgres" {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('chatgpt2api_image_task_base64_v1'))"); err != nil {
			return err
		}
	}
	var marker string
	if err := tx.QueryRow("SELECT name FROM app_migrations WHERE name = 'image_task_base64_v1'").Scan(&marker); err == nil {
		return tx.Commit()
	} else if err != sql.ErrNoRows {
		return err
	}
	var payload string
	err = tx.QueryRow("SELECT payload FROM app_documents WHERE name = "+s.bind(1), "image_tasks.json").Scan(&payload)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		var tasks []ImageTask
		if err := json.Unmarshal([]byte(payload), &tasks); err != nil {
			return err
		}
		changed := false
		for i := range tasks {
			for _, result := range tasks[i].Data {
				encoded := strings.TrimSpace(strAny(result["b64_json"], ""))
				if encoded == "" {
					continue
				}
				image, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return fmt.Errorf("decode image task %s: %w", tasks[i].ID, err)
				}
				rel := imageTaskResultRel(strAny(result["url"], ""), image)
				path := filepath.Join(s.dir, "images", filepath.FromSlash(rel))
				if _, err := os.Stat(path); os.IsNotExist(err) {
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						return err
					}
					if err := writeMigratedImage(path, image); err != nil {
						return err
					}
				} else if err != nil {
					return err
				}
				result["image_rel"] = rel
				result["url"] = "/images/" + rel
				delete(result, "b64_json")
				changed = true
			}
		}
		if changed {
			data, err := json.Marshal(tasks)
			if err != nil {
				return err
			}
			query := "UPDATE app_documents SET payload = " + s.bind(1) + " WHERE name = " + s.bind(2)
			if _, err := tx.Exec(query, string(data), "image_tasks.json"); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec("INSERT INTO app_migrations (name) VALUES ('image_task_base64_v1') ON CONFLICT (name) DO NOTHING"); err != nil {
		return err
	}
	return tx.Commit()
}

func writeMigratedImage(path string, image []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".image-task-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(image); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func imageTaskResultRel(rawURL string, image []byte) string {
	if parsed, err := url.Parse(rawURL); err == nil && strings.HasPrefix(parsed.Path, "/images/") {
		if rel, err := safeImageRel(strings.TrimPrefix(parsed.Path, "/images/")); err == nil {
			return rel
		}
	}
	return fmt.Sprintf("legacy-tasks/%x.png", md5.Sum(image))
}
