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

func TestRecoveredCandidatePasswordConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, eligibility, outcome string
		add, success               bool
	}{
		{"add-missing-password", `{"eligible":true}`, "success", true, true},
		{"reset-unknown-password", `{"eligible":false}`, "success", false, true},
		{"setup-failure", `{"eligible":true}`, "failure", true, false},
		{"missing-session", `{"eligible":true}`, "no-token", true, false},
		{"wrong-account", `{"eligible":true}`, "wrong-email", true, false},
		{"invalid-eligibility", `{}`, "unused", false, false},
		{"concurrent-password-edit", `{"eligible":true}`, "edit", true, false},
		{"accepted-password-survives-login-failure", `{"eligible":false}`, "accepted-then-failed", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email, password, rt := "partial@icloud.com", "candidate-password", "platform-rt"
			old := Account{Email: &email, Password: &password, AccessToken: "old-at", RefreshToken: &rt, Status: "异常", MFAStatus: "pending"}
			if err := s.store.SaveAccounts([]Account{old}); err != nil {
				t.Fatal(err)
			}
			if err := s.register.savePendingPassword(email, password, "待确认"); err != nil {
				t.Fatal(err)
			}
			session := &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.Path != "/backend-api/accounts/add_password/eligibility" || r.Header.Get("Authorization") != "Bearer old-at" {
					t.Error("unexpected eligibility request")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.eligibility)), Request: r}, nil
			})}}
			called := false
			s.register.passwordBrowserFactory = func(_ context.Context, browserSession *registerSession, account Account, add bool, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				called = true
				if add != tc.add || account.PasswordStatus == "confirmed" || s.store.LoadAccounts()[0].Status != "异常" {
					t.Fatal("candidate was prematurely confirmed or wrong password flow selected")
				}
				if tc.outcome == "failure" {
					return nil, errors.New("password submission failed")
				}
				if tc.outcome == "accepted-then-failed" {
					if browserSession.onPasswordAccepted == nil {
						t.Fatal("missing password persistence hook")
					}
					if err := browserSession.onPasswordAccepted(); err != nil {
						t.Fatal(err)
					}
					if s.store.LoadAccounts()[0].PasswordStatus != "confirmed" {
						t.Fatal("password was not saved before login")
					}
					return nil, errors.New("password accepted but login failed")
				}
				if tc.outcome == "no-token" {
					return map[string]any{"email": email}, nil
				}
				if tc.outcome == "wrong-email" {
					return map[string]any{"email": "other@icloud.com", "access_token": "browser-at"}, nil
				}
				if err := s.store.UpdateAccounts(func(items []Account) []Account {
					items[0].Success = 7
					if tc.outcome == "edit" {
						changed := "new-candidate"
						items[0].Password = &changed
						items[0].PasswordStatus = "unverified"
					}
					return items
				}); err != nil {
					t.Fatal(err)
				}
				return map[string]any{"access_token": "browser-at", "email": email}, nil
			}
			_, err := s.register.confirmRecoveredPassword(context.Background(), session, old, nil)
			if (err == nil) != tc.success || called != (tc.outcome != "unused") {
				t.Fatalf("unexpected confirmation outcome: success=%t called=%t", err == nil, called)
			}
			current := s.store.LoadAccounts()[0]
			if current.Status != "异常" {
				t.Fatal("password step must not complete pending MFA")
			}
			if tc.success {
				if current.PasswordStatus != "confirmed" || current.AccessToken != "browser-at" || current.RefreshToken != nil || current.Success != 7 {
					t.Fatal("confirmed credentials or concurrent metadata not saved correctly")
				}
			} else if (current.PasswordStatus == "confirmed") != (tc.outcome == "accepted-then-failed") || current.AccessToken != "old-at" || ptrString(current.RefreshToken) != rt {
				t.Fatal("failed setup changed existing credentials or confirmed a candidate")
			}
			if s.register.pendingPasswordForEmail(email) == "" {
				t.Fatal("password step must preserve recovery until the coordinator completes")
			}
		})
	}
}

func TestPasswordReuseRejectionAloneDoesNotConfirmCandidate(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	email, password := "already-used@icloud.com", "candidate-password"
	account := Account{Email: &email, Password: &password, AccessToken: "old-at", Status: "异常", PasswordStatus: "unverified", MFAStatus: "pending"}
	if err := s.store.SaveAccounts([]Account{account}); err != nil {
		t.Fatal(err)
	}
	session := &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"eligible":true}`)), Request: r}, nil
	})}}
	s.register.passwordBrowserFactory = func(context.Context, *registerSession, Account, bool, func(context.Context, time.Time) (string, error)) (map[string]any, error) {
		return nil, errors.New("账号接口请求未被接受: HTTP 400 code=password_already_used")
	}
	got, err := s.register.confirmRecoveredPassword(context.Background(), session, account, nil)
	if err == nil || got.PasswordStatus == "confirmed" || got.AccessToken != "old-at" {
		t.Fatal("password reuse rejection was treated as proof of the current password")
	}
	current := s.store.LoadAccounts()[0]
	if current.PasswordStatus == "confirmed" || current.Status != "异常" {
		t.Fatalf("persisted candidate status=%+v", current)
	}
}

func TestCandidateAndLegacyPasswordDoNotClearRecovery(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	email, password, secret := "partial@icloud.com", "candidate-password", "mfa-secret"
	for _, status := range []string{"", "unverified", "confirmed"} {
		if err := s.register.savePendingPassword(email, password, "提交待确认"); err != nil {
			t.Fatal(err)
		}
		if err := s.register.recordRecoveryEmail(email, "未完成"); err != nil {
			t.Fatal(err)
		}
		s.clearCompletedRegisterRecovery([]Account{{Email: &email, Password: &password, PasswordStatus: status, MFAStatus: "enabled", MFASecret: &secret}})
		if want := status != "confirmed"; (s.register.pendingPasswordForEmail(email) != "") != want || s.register.hasRecoveryEmail(email) != want {
			t.Fatalf("incorrect cleanup for password status %q", status)
		}
	}
	legacy := accountFromRecord("token", "web", map[string]any{"password": password})
	if legacy.PasswordStatus != "unverified" {
		t.Fatal("legacy password must not be treated as confirmed")
	}
}

func TestPendingMFASecretDoesNotClearRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		secret       bool
		cleared      bool
	}{
		{"pending enrollment", "pending", true, false},
		{"failed activation", "failed", true, false},
		{"legacy enrollment", "", true, false},
		{"enabled without saved secret", "enabled", false, false},
		{"activated and saved", "enabled", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email, password, secret := "partial@icloud.com", "confirmed-password", "enrollment-secret"
			if err := s.register.savePendingPassword(email, password, "密码已接受"); err != nil {
				t.Fatal(err)
			}
			if err := s.register.recordRecoveryEmail(email, "账号已入池，2FA 待完成"); err != nil {
				t.Fatal(err)
			}
			a := Account{AccessToken: "existing-token", Email: &email, Password: &password, PasswordStatus: "confirmed", Status: "异常", MFAStatus: tc.status}
			if tc.secret {
				a.MFASecret = &secret
			}
			if err := s.store.SaveAccounts([]Account{a}); err != nil {
				t.Fatal(err)
			}
			// Imports and unrelated account edits call the same cleanup path.
			s.clearCompletedRegisterRecovery(s.store.LoadAccounts())
			reloaded := newRegisterService(t.TempDir(), s.store)
			if reloaded.hasRecoveryEmail(email) == tc.cleared {
				t.Fatalf("recovery lost across restart for MFA status %q", tc.status)
			}
			if reloaded.pendingPasswordForEmail(email) != "" || ptrString(s.store.LoadAccounts()[0].Password) != password {
				t.Fatal("confirmed password was not retained on the pooled account")
			}
		})
	}
}
