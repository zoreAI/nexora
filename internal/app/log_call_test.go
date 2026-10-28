package app

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCallLogUpdatesSingleRow(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "data"))
	defer store.Close()
	s := &Server{logSvc: newLogService(filepath.Join(root, "data")), callStarts: map[string]time.Time{}}
	s.logSvc.store = store
	callID := s.logCallStart(nil, "/v1/images/edits", "image", "图生图", "original prompt")
	s.logCallSuccess(callID, "/v1/images/edits", "image", "图生图", map[string]any{"image_count": 1})
	items := s.logSvc.list("call", "", "", 10)
	if len(items) != 1 || items[0]["id"] != callID {
		t.Fatalf("success logs = %+v", items)
	}
	detail := items[0]["detail"].(map[string]any)
	if detail["status"] != "success" || detail["request_text"] != "original prompt" || detail["image_count"] != float64(1) || detail["duration_ms"] == nil {
		t.Fatalf("success detail = %+v", detail)
	}
	failedID := s.logCallStart(nil, "/v1/images/edits", "image", "图生图", "failed prompt")
	s.logCallFailure(failedID, "/v1/images/edits", "image", "图生图", errors.New("upload failed"), nil)
	items = s.logSvc.list("call", "", "", 10)
	if len(items) != 2 {
		t.Fatalf("failure added another row: %+v", items)
	}
	if got := s.logSvc.listFiltered("call", "", "", "failed", "", "", "upload failed", 10); len(got) != 1 {
		t.Fatalf("failure filter = %+v", got)
	}
}

func TestMergeLegacyCallLogs(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "data"))
	defer store.Close()
	logs := newLogService(filepath.Join(root, "data"))
	logs.store = store
	logs.add("call", "图生图开始", map[string]any{"call_id": "legacy-call", "status": "started", "request_text": "prompt", "endpoint": "/v1/images/edits"})
	logs.add("call", "图生图完成", map[string]any{"call_id": "legacy-call", "status": "success", "duration_ms": 1234, "endpoint": "/v1/images/edits"})
	if _, err := store.db.Exec("DELETE FROM app_migrations WHERE name = 'call_logs_v1'"); err != nil {
		t.Fatal(err)
	}
	if err := store.mergeLegacyCallLogs(); err != nil {
		t.Fatal(err)
	}
	items := logs.list("call", "", "", 10)
	if len(items) != 1 {
		t.Fatalf("legacy logs = %+v", items)
	}
	detail := items[0]["detail"].(map[string]any)
	if detail["status"] != "success" || detail["request_text"] != "prompt" || detail["duration_ms"] != float64(1234) {
		t.Fatalf("legacy detail = %+v", detail)
	}
}
