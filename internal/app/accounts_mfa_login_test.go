package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountMFALoginRequiresAdminAndEligibleAccount(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"auth-key":"admin-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.Close()
	secret, password, email := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "password", "test@example.com"
	if err := s.store.SaveAccounts([]Account{{AccessToken: "token", Status: "正常", MFAStatus: "enabled", MFASecret: &secret, Password: &password, Email: &email}}); err != nil {
		t.Fatal(err)
	}
	request := func(token string, admin bool) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/mfa/login", strings.NewReader(`{"access_token":"`+token+`"}`))
		if admin {
			req.Header.Set("Authorization", "Bearer admin-key")
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request("token", false); got != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", got)
	}
	if got := request("missing", true); got != http.StatusNotFound {
		t.Fatalf("missing status = %d", got)
	}
	if got := request("token", true); got != http.StatusBadRequest {
		t.Fatalf("normal account status = %d", got)
	}
}
