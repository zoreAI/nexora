package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountRepairStartOnlySelectsAbnormalAccounts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"auth-key":"admin-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.Close()
	if err := s.store.SaveAccounts([]Account{
		{AccessToken: "normal", Status: "正常"},
		{AccessToken: "abnormal", Status: "异常"},
	}); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if admin {
			req.Header.Set("Authorization", "Bearer admin-key")
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	path := "/api/accounts/repair/start"
	body := `{"access_tokens":["normal","abnormal","abnormal"]}`
	if rec := request(http.MethodPost, path, body, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous start status=%d", rec.Code)
	}
	if rec := request(http.MethodPost, path, `{"access_tokens":["normal"]}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("normal-only start status=%d", rec.Code)
	}
	rec := request(http.MethodPost, path, body, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status=%d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID    string `json:"progress_id"`
		Total int    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" || started.Total != 1 {
		t.Fatalf("start response=%s err=%v", rec.Body.String(), err)
	}
	progressPath := "/api/accounts/repair/progress/" + started.ID
	if rec := request(http.MethodGet, progressPath, "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous progress status=%d", rec.Code)
	}
	deadline := time.Now().Add(time.Second)
	for {
		rec = request(http.MethodGet, progressPath, "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("progress status=%d", rec.Code)
		}
		var progress accountRefreshProgress
		if err := json.Unmarshal(rec.Body.Bytes(), &progress); err != nil {
			t.Fatal(err)
		}
		if progress.Done {
			if progress.Total != 1 || progress.Processed != 1 || progress.Failed != 1 || progress.Refreshed != 0 {
				t.Fatalf("unexpected progress: %+v", progress)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("repair job did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if accounts := s.store.LoadAccounts(); len(accounts) != 2 || accounts[0].Status != "正常" || accounts[1].Status != "异常" {
		t.Fatalf("repair changed unrelated accounts: %+v", accounts)
	}
}
