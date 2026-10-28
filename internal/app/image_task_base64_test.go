package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageTaskResultsUseURLWithoutBase64(t *testing.T) {
	s := testImageStorageServer(t, "local", "")
	results := []upstreamImageResult{{Bytes: testPNG(t)}}
	data, rels, err := s.saveImageTaskResults(context.Background(), "https://app.example.test", "prompt", results)
	if err != nil || len(data) != 1 || len(rels) != 1 {
		t.Fatalf("save task result: data=%v rels=%v err=%v", data, rels, err)
	}
	if _, ok := data[0]["b64_json"]; ok || data[0]["url"] != "/images/"+rels[0] {
		t.Fatalf("task result contains base64 or external URL: %+v", data[0])
	}
}

func TestMigrateImageTaskBase64PreservesImages(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := NewStore(dir)
	defer store.Close()
	image := testPNG(t)
	encoded := base64.StdEncoding.EncodeToString(image)
	tasks := []ImageTask{
		{ID: "missing", Data: []map[string]any{{"url": "http://localhost:3000/images/2026/10/missing.png", "b64_json": encoded}}},
		{ID: "invalid-url", Data: []map[string]any{{"b64_json": encoded}}},
	}
	if err := store.SaveTasks(tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DELETE FROM app_migrations WHERE name = 'image_task_base64_v1'"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateImageTaskBase64(); err != nil {
		t.Fatal(err)
	}
	for _, task := range store.LoadTasks() {
		result := task.Data[0]
		if _, ok := result["b64_json"]; ok {
			t.Fatalf("base64 remains in task %s", task.ID)
		}
		url := strAny(result["url"], "")
		if !strings.HasPrefix(url, "/images/") {
			t.Fatalf("task %s URL = %q", task.ID, url)
		}
		path := filepath.Join(dir, "images", filepath.FromSlash(strings.TrimPrefix(url, "/images/")))
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, image) {
			t.Fatalf("task %s image was not preserved: %v", task.ID, err)
		}
	}
	if err := store.migrateImageTaskBase64(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}
