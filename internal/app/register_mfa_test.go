package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type mfaRoundTripper func(*http.Request) (*http.Response, error)

func (fn mfaRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestMFAFailurePreservesAccountClassification(t *testing.T) {
	for _, tc := range []struct {
		name, code, wantStatus string
		status                 int
	}{
		{"expired", "token_expired", "异常", 401},
		{"deactivated", "account_deactivated", "封号", 403},
		{"temporary", "server_error", "异常", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			password := "saved-password"
			if err := s.store.SaveAccounts([]Account{{AccessToken: "saved-token", Password: &password, Status: "正常", MFAStatus: "failed"}}); err != nil {
				t.Fatal(err)
			}
			session := &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(*http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"error":{"code":%q},"secret":"must-not-leak","session_id":"must-not-leak"}`, tc.code)
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			err := s.register.enableRegisterMFA(context.Background(), session, "saved-token", "device")
			got := s.store.LoadAccounts()[0]
			if err == nil || strings.Contains(err.Error(), "must-not-leak") || got.Status != tc.wantStatus || got.MFAStatus != "failed" || ptrString(got.Password) != password || got.AccessToken != "saved-token" {
				t.Fatalf("incorrect MFA failure handling: err=%v status=%s mfa=%s", err, got.Status, got.MFAStatus)
			}
			if tc.code == "account_deactivated" && !isAccountDeactivatedError(err) {
				t.Fatal("deactivation code was lost")
			}
		})
	}
}

func TestRegisterTOTP(t *testing.T) {
	code, err := registerTOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	if err != nil || code != "287082" {
		t.Fatalf("code = %q, err = %v", code, err)
	}
	if _, err := registerTOTP("invalid!", time.Now()); err == nil {
		t.Fatal("invalid secret accepted")
	}
}

func TestUnsupportedCountryIsEligibleForProxyFailover(t *testing.T) {
	if !isTransientRegisterNetworkError(errors.New(`GET authorize failed: status=403 code=unsupported_country_region_territory`)) {
		t.Fatal("unsupported country rejection should rotate the egress proxy")
	}
}

func TestRegisterMFAEnrollmentSwitchesProxyAfterConnectionReset(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "register", Strategy: "round_robin", Nodes: []proxyNodeInput{
		{URL: "http://127.0.0.1:7801"}, {URL: "http://127.0.0.1:7802"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "异常", MFAStatus: "pending"}}); err != nil {
		t.Fatal(err)
	}
	firstCalls, secondCalls := 0, 0
	first := &registerSession{
		proxyRoute: proxyRoute{URL: group.Nodes[0].URL, NodeID: group.Nodes[0].ID},
		client: &http.Client{Transport: mfaRoundTripper(func(*http.Request) (*http.Response, error) {
			firstCalls++
			return nil, syscall.ECONNRESET
		})},
	}
	svc := &registerService{store: store, server: &Server{store: store}, cfg: RegisterConfig{Proxy: "group:" + group.ID}}
	svc.mfaSessionFactory = func(proxyURL string) (*registerSession, error) {
		if proxyURL != group.Nodes[1].URL {
			t.Fatalf("fallback proxy=%q", proxyURL)
		}
		return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			secondCalls++
			response := `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`
			if strings.HasSuffix(req.URL.Path, "/activate_enrollment") {
				response = `{"success":true}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
		})}}, nil
	}
	if err := svc.enableRegisterMFAWithFailover(context.Background(), first, "token", "device"); err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || secondCalls != 2 {
		t.Fatalf("calls on failed/healthy nodes=%d/%d", firstCalls, secondCalls)
	}
	groups, err := store.LoadProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	if groups[0].Nodes[0].Health.State != "unhealthy" || groups[0].Nodes[1].Health.State != "healthy" {
		t.Fatalf("proxy health after failover=%v/%v", groups[0].Nodes[0].Health.State, groups[0].Nodes[1].Health.State)
	}
	if route, err := svc.resolveRegisterProxy("group:" + group.ID); err != nil || route.NodeID != group.Nodes[1].ID {
		t.Fatalf("next registration route=%+v err=%v", route, err)
	}
	if account := store.LoadAccounts()[0]; account.MFAStatus != "enabled" || account.Status != "正常" {
		t.Fatalf("account after failover: status=%q mfa=%q", account.Status, account.MFAStatus)
	}
}

func TestRegisterMFAActivationSwitchesProxyWithoutReenrollment(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "register", Nodes: []proxyNodeInput{
		{URL: "http://127.0.0.1:7811"}, {URL: "http://127.0.0.1:7812"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "异常", MFAStatus: "pending"}}); err != nil {
		t.Fatal(err)
	}
	first := &registerSession{
		proxyRoute: proxyRoute{URL: group.Nodes[0].URL, NodeID: group.Nodes[0].ID},
		client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/enroll") {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`)), Header: http.Header{}}, nil
			}
			return nil, syscall.ECONNRESET
		})},
	}
	svc := &registerService{store: store, server: &Server{store: store}, cfg: RegisterConfig{Proxy: "group:" + group.ID}}
	svc.mfaSessionFactory = func(proxyURL string) (*registerSession, error) {
		return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(req.URL.Path, "/activate_enrollment") {
				t.Errorf("unexpected reenrollment on fallback: %s", req.URL.Path)
			}
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["session_id"] != "session" || len(body["code"]) != 6 {
				t.Errorf("activation body=%v err=%v", body, err)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"success":true}`)), Header: http.Header{}}, nil
		})}}, nil
	}
	if err := svc.enableRegisterMFAWithFailover(context.Background(), first, "token", "device"); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterMFAEnrollDoesNotRetryHTTPError(t *testing.T) {
	calls := 0
	session := &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})}}
	_, err := session.registerMFARequest(context.Background(), http.MethodPost, "/enroll", "token", "device", nil)
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("HTTP error retry: calls=%d err=%v", calls, err)
	}
}

func TestFailedMFAEnrollmentClearsUnactivatedSecret(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if err := store.SaveAccounts([]Account{{AccessToken: "token", MFAStatus: "pending", MFASecret: &secret}}); err != nil {
		t.Fatal(err)
	}
	if err := store.updateAccountMFA("token", "failed", ""); err != nil {
		t.Fatal(err)
	}
	account := store.LoadAccounts()[0]
	if account.MFAStatus != "failed" || account.MFASecret != nil {
		t.Fatalf("failed enrollment retained inactive secret: status=%q secret_present=%t", account.MFAStatus, account.MFASecret != nil)
	}
}

func TestAccountMFASetupRejectsAccountWithoutFailedEnrollment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"auth-key":"admin-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.Close()
	if err := s.store.SaveAccounts([]Account{{AccessToken: "token", MFAStatus: "enabled"}}); err != nil {
		t.Fatal(err)
	}
	request := func(admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/mfa/setup", strings.NewReader(`{"access_token":"token"}`))
		if admin {
			req.Header.Set("Authorization", "Bearer admin-key")
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if got := request(false).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", got)
	}
	if got := request(true).Code; got != http.StatusConflict {
		t.Fatalf("already enabled status=%d", got)
	}
}

func TestImportRegisterAccountConcurrent(t *testing.T) {
	store := NewStore(t.TempDir())
	svc := &registerService{server: &Server{store: store}}
	var group sync.WaitGroup
	errors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			errors <- svc.importRegisterAccount(map[string]any{
				"access_token": fmt.Sprintf("token-%d", index),
				"email":        fmt.Sprintf("user-%d@example.com", index),
			})
		}(i)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.LoadAccounts()); got != 20 {
		t.Fatalf("saved accounts = %d, want 20", got)
	}
}

func TestAccountMFACodeRequiresAdminAndEnabledStatus(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"auth-key":"admin-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if err := s.store.SaveAccounts([]Account{{AccessToken: "token", Status: "正常", MFAStatus: "enabled", MFASecret: &secret}}); err != nil {
		t.Fatal(err)
	}
	request := func(admin bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/mfa/code", strings.NewReader(`{"access_token":"token"}`))
		if admin {
			req.Header.Set("Authorization", "Bearer admin-key")
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := request(false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", rec.Code)
	}
	rec := request(true)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || len(result.Code) != 6 {
		t.Fatalf("code response invalid: %v", err)
	}
	if err := s.store.updateAccountMFA("token", "pending", secret); err != nil {
		t.Fatal(err)
	}
	if rec := request(true); rec.Code != http.StatusConflict {
		t.Fatalf("pending status = %d", rec.Code)
	}
}

func TestRegisterMFAActivationAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name              string
		activation        string
		wantStatus        string
		wantAccountStatus string
		wantError         bool
	}{
		{"enabled", `{"success":true}`, "enabled", "正常", false},
		{"not confirmed", `{"success":false}`, "pending", "异常", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := NewStore(root)
			if err := store.SaveAccounts([]Account{{AccessToken: "test-token", Status: "异常", MFAStatus: "pending"}}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			session := &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("authorization header missing")
				}
				var body map[string]string
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				response := `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`
				if strings.HasSuffix(req.URL.Path, "/enroll") {
					if body["factor_type"] != "totp" || body["source"] != "settings" {
						t.Errorf("enroll body = %v", body)
					}
				} else if strings.HasSuffix(req.URL.Path, "/activate_enrollment") {
					if len(body["code"]) != 6 || body["factor_type"] != "totp" || body["session_id"] != "session" {
						t.Errorf("activation body = %v", body)
					}
					response = tc.activation
				} else {
					t.Errorf("unexpected path %s", req.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
			})}}
			svc := &registerService{server: &Server{store: store}}
			err := svc.enableRegisterMFA(context.Background(), session, "test-token", "device-id")
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if calls != 2 {
				t.Fatalf("calls = %d", calls)
			}
			account := store.LoadAccounts()[0]
			if account.MFAStatus != tc.wantStatus || account.Status != tc.wantAccountStatus || account.MFASecret == nil || *account.MFASecret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
				t.Fatalf("MFA/account status or secret not saved: mfa_status=%q account_status=%q secret_present=%t", account.MFAStatus, account.Status, account.MFASecret != nil)
			}
			info, err := os.Stat(filepath.Join(root, "chatgpt2api.db"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("chatgpt2api.db mode = %v", info.Mode().Perm())
			}
			store.Close()
			reopened := NewStore(root)
			defer reopened.Close()
			if got := reopened.LoadAccounts(); len(got) != 1 || got[0].MFAStatus != tc.wantStatus {
				t.Fatalf("MFA status after reopen = %+v", got)
			}
		})
	}
}
