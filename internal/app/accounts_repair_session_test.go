package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRepairExistingSessionCompletesPasswordAndMFA(t *testing.T) {
	for _, failPassword := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "keep-partial-on-failure"}[failPassword], func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email, password := "partial@example.com", "candidate"
			account := Account{AccessToken: "existing-token", Email: &email, Password: &password, SourceType: "web", Status: "异常", PasswordStatus: "unverified", MFAStatus: "pending"}
			if err := s.store.SaveAccounts([]Account{account}); err != nil {
				t.Fatal(err)
			}
			if err := s.register.savePendingPassword(email, password, "待接续"); err != nil {
				t.Fatal(err)
			}
			passwordCalls, enrolls := 0, 0
			factory := func(string, int) (*registerSession, error) {
				return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(r *http.Request) (*http.Response, error) {
					body := `{}`
					switch r.URL.Path {
					case "/backend-api/me":
						body = `{"email":"PARTIAL@example.com"}`
					case "/backend-api/accounts/add_password/eligibility":
						body = `{"eligible":true}`
					case "/backend-api/accounts/mfa/enroll":
						enrolls++
						if s.store.LoadAccounts()[0].PasswordStatus != "confirmed" {
							t.Error("MFA before password persistence")
						}
						body = `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"test"}`
					case "/backend-api/accounts/mfa/user/activate_enrollment":
						body = `{"success":true}`
					default:
						t.Errorf("unexpected request, valid session must not reauthorize: %s", r.URL.Path)
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}}, nil
			}
			s.accountLoginSessionFactory = factory
			s.register.mfaSessionFactory = func(p string) (*registerSession, error) { return factory(p, 45) }
			s.register.passwordBrowserFactory = func(_ context.Context, _ *registerSession, a Account, add bool, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				passwordCalls++
				if !add || a.AccessToken != account.AccessToken {
					t.Fatal("wrong account/password flow")
				}
				if failPassword {
					return nil, errors.New("mail unavailable")
				}
				return map[string]any{"access_token": "password-token", "email": email}, nil
			}
			err := s.repairAbnormalAccount(context.Background(), account)
			got := s.store.LoadAccounts()[0]
			if passwordCalls != 1 {
				t.Fatal("password setup not called once")
			}
			if failPassword {
				if err == nil || got.Status != "异常" || enrolls != 0 || !s.hasRegisterRecovery(got) {
					t.Fatal("failed password setup lost recovery state")
				}
			} else if err != nil || got.Status != "正常" || got.PasswordStatus != "confirmed" || got.MFAStatus != "enabled" || ptrString(got.MFASecret) == "" || enrolls != 1 || s.hasRegisterRecovery(got) {
				t.Fatalf("incomplete repair: err=%v status=%s", err, got.Status)
			}
		})
	}
}

func TestValidateRecoverySessionDoesNotTrustWrongIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status       int
		transportErr bool
		wantErr      bool
	}{
		{"same-email", `{"email":"partial@example.com"}`, 200, false, false},
		{"wrong-email", `{"email":"other@example.com"}`, 200, false, true},
		{"missing-email", `{}`, 200, false, true},
		{"invalid-json", `not-json`, 200, false, true},
		{"expired", `{"error":{"code":"token_expired"}}`, 401, false, false},
		{"deactivated", `{"error":{"code":"account_deactivated"}}`, 403, false, true},
		{"transport-error", ``, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email := "partial@example.com"
			s.accountLoginSessionFactory = func(string, int) (*registerSession, error) {
				return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("Authorization") != "Bearer saved-token" {
						t.Error("missing saved token")
					}
					if tc.transportErr {
						return nil, errors.New("connection reset by peer")
					}
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
				})}}, nil
			}
			valid, err := s.validateRecoverySession(context.Background(), Account{AccessToken: "saved-token", Email: &email})
			if (err != nil) != tc.wantErr || valid != (tc.name == "same-email") {
				t.Fatalf("valid=%v err=%v", valid, err)
			}
		})
	}
}
