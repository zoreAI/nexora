package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestRepairBrowserFallbackContinuesPasswordAndMFA(t *testing.T) {
	for _, tc := range []struct {
		name            string
		passwordTrusted bool
		mfaEnabled      bool
		browserFails    bool
		mfaFails        bool
	}{
		{name: "confirm-password-and-activate-mfa"},
		{name: "password-confirmed-by-browser", passwordTrusted: true},
		{name: "keep-existing-mfa", mfaEnabled: true},
		{name: "preserve-token-after-mfa-failure", passwordTrusted: true, mfaFails: true},
		{name: "preserve-original-on-browser-failure", browserFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email, password, rt := "recovery@example.com", "candidate-password", "old-refresh"
			old := Account{AccessToken: "old-token", Email: &email, Password: &password, RefreshToken: &rt, SourceType: "web", Status: "异常", PasswordStatus: "unverified", MFAStatus: "pending"}
			if tc.mfaEnabled {
				old.MFAStatus = "enabled"
				secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
				old.MFASecret = &secret
			}
			if err := s.store.SaveAccounts([]Account{old}); err != nil {
				t.Fatal(err)
			}
			if err := s.register.savePendingPassword(email, password, "待修复"); err != nil {
				t.Fatal(err)
			}
			browserCalls, passwordCalls, mfaCalls := 0, 0, 0
			s.register.signupBrowserFactory = func(_ context.Context, _ *registerSession, gotEmail, gotPassword string, _ registerProfileConfig, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				browserCalls++
				if gotEmail != email || gotPassword != password {
					t.Fatal("browser did not receive original account")
				}
				if tc.browserFails {
					return nil, errors.New("browser transition failed")
				}
				return map[string]any{"access_token": "browser-token", "email": email, "password_confirmed": tc.passwordTrusted}, nil
			}
			s.register.passwordBrowserFactory = func(_ context.Context, _ *registerSession, account Account, add bool, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				passwordCalls++
				current := s.store.LoadAccounts()[0]
				if !add || current.AccessToken != "browser-token" || current.Status != "异常" {
					t.Fatal("password setup ran without saving recovered token")
				}
				return map[string]any{"access_token": "password-token", "email": ptrString(account.Email)}, nil
			}
			s.accountLoginSessionFactory = func(string, int) (*registerSession, error) {
				jar, _ := cookiejar.New(nil)
				return &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
					status, body := 200, `{}`
					headers := make(http.Header)
					switch req.URL.Path {
					case "/backend-api/me":
						status, body = 401, `{"error":{"code":"token_expired"}}`
					case "/api/accounts/authorize":
						status = 302
						headers.Set("Location", registerAuthBase+"/log-in/password")
					case "/log-in/password":
						body = "<html></html>"
					case "/backend-api/sentinel/req":
						body = `{"token":"sentinel-test"}`
					case "/api/accounts/password/verify":
						status, body = 409, `{"error":{"code":"invalid_state","message":"Your sign-in session is no longer valid"}}`
					case "/backend-api/accounts/add_password/eligibility":
						body = `{"eligible":true}`
					default:
						t.Errorf("unexpected login request: %s", req.URL.Path)
						status = 400
					}
					return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}}, nil
			}
			s.register.mfaSessionFactory = func(string) (*registerSession, error) {
				return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
					body := `{"success":true}`
					if strings.HasSuffix(req.URL.Path, "/enroll") {
						mfaCalls++
						current := s.store.LoadAccounts()[0]
						if current.PasswordStatus != "confirmed" || current.Status != "异常" {
							t.Error("MFA ran before password confirmation")
						}
						if tc.mfaFails {
							return nil, errors.New("enrollment transport failed")
						}
						body = `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"test-enroll"}`
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}}, nil
			}
			err := s.repairAbnormalAccount(context.Background(), old)
			current := s.store.LoadAccounts()[0]
			if browserCalls != 1 {
				t.Fatalf("browser calls=%d", browserCalls)
			}
			if tc.browserFails {
				if err == nil || !strings.Contains(err.Error(), "browser transition failed") || current.AccessToken != old.AccessToken || current.Status != "异常" || passwordCalls != 0 || mfaCalls != 0 {
					t.Fatal("browser failure did not preserve original account")
				}
				return
			}
			wantPasswordCalls := 1
			wantToken := "password-token"
			if tc.passwordTrusted {
				wantPasswordCalls, wantToken = 0, "browser-token"
			}
			if current.AccessToken != wantToken || current.PasswordStatus != "confirmed" || current.RefreshToken != nil || passwordCalls != wantPasswordCalls {
				t.Fatal("browser recovery did not finish password setup or retained stale refresh token")
			}
			if tc.mfaFails {
				if err == nil || current.Status != "异常" || current.MFAStatus == "enabled" || mfaCalls == 0 {
					t.Fatal("MFA failure lost recovered credentials or marked account normal")
				}
				return
			}
			if err != nil || current.Status != "正常" || current.MFAStatus != "enabled" || (mfaCalls == 0) != tc.mfaEnabled || s.hasRegisterRecovery(current) {
				t.Fatalf("browser fallback did not finish recovery: err=%v status=%s mfa=%s", err, current.Status, current.MFAStatus)
			}
		})
	}
}
