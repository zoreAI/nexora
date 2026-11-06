package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountsUpdateLoginCredentials(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	s := &Server{cfg: Config{AuthKey: "admin-key"}, store: store, mux: http.NewServeMux()}
	s.routes()
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "异常"}}); err != nil {
		t.Fatal(err)
	}
	request := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/update", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request(`{"access_token":"token","password":"new-password","mfa_secret":"gezd gnbv gy3t qojq gezd gnbv gy3t qojq"}`); got != http.StatusOK {
		t.Fatalf("save credentials status = %d", got)
	}
	account := store.LoadAccounts()[0]
	if ptrString(account.Password) != "new-password" || ptrString(account.MFASecret) != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" || account.MFAStatus != "enabled" {
		t.Fatal("credentials were not saved and normalized")
	}
	if got := request(`{"access_token":"token","status":"正常"}`); got != http.StatusOK {
		t.Fatalf("update without credentials status = %d", got)
	}
	account = store.LoadAccounts()[0]
	if ptrString(account.Password) != "new-password" || ptrString(account.MFASecret) != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatal("unrelated update removed credentials")
	}
	if got := request(`{"access_token":"token","mfa_secret":"invalid!"}`); got != http.StatusBadRequest {
		t.Fatalf("invalid 2FA secret status = %d", got)
	}
	if ptrString(store.LoadAccounts()[0].MFASecret) != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatal("invalid secret changed stored credential")
	}
}

func TestEditingSamePasswordPreservesAcceptedStatus(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	password := "accepted-password"
	if err := s.store.SaveAccounts([]Account{{AccessToken: "token", Password: &password, PasswordStatus: "confirmed", Status: "正常"}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/update", strings.NewReader(`{"access_token":"token","password":"accepted-password"}`))
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	s.handleAccountsUpdate(rec, req)
	if rec.Code != 200 || s.store.LoadAccounts()[0].PasswordStatus != "confirmed" {
		t.Fatalf("unchanged password lost acceptance: status=%d", rec.Code)
	}
}
