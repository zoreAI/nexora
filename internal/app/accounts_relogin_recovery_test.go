package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPooledAbnormalRepairFollowsCurrentAuthorizationStep(t *testing.T) {
	for _, tc := range []struct {
		name, landing, mfa, continuation           string
		recovery, missingPassword, normal, failAdd bool
	}{
		{name: "existing-email-login", landing: "/email-verification", continuation: "https://platform.openai.com/auth/callback?code=oauth-code"},
		{name: "partial-email-profile", landing: "/email-verification", mfa: "pending", continuation: "https://auth.openai.com/about-you"},
		{name: "partial-password-signup", landing: "/create-account/password", mfa: "failed", continuation: "https://auth.openai.com/about-you"},
		{name: "registered-without-mfa-status", landing: "/email-verification", continuation: "https://platform.openai.com/auth/callback?code=oauth-code", recovery: true},
		{name: "registered-without-password", landing: "/email-verification", continuation: "https://platform.openai.com/auth/callback?code=oauth-code", recovery: true, missingPassword: true},
		{name: "normal-but-registration-incomplete", landing: "/email-verification", continuation: "https://platform.openai.com/auth/callback?code=oauth-code", recovery: true, normal: true},
		{name: "registered-mfa-already-enabled", landing: "/email-verification", mfa: "enabled", continuation: "https://platform.openai.com/auth/callback?code=oauth-code", recovery: true},
		{name: "password-add-fails", landing: "/email-verification", continuation: "https://platform.openai.com/auth/callback?code=oauth-code", recovery: true, missingPassword: true, failAdd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			group, err := s.store.SaveProxyGroup(proxyGroupInput{Name: "repair", Strategy: "round_robin", Nodes: []proxyNodeInput{
				{Name: "first", URL: "http://127.0.0.1:18001"}, {Name: "second", URL: "http://127.0.0.1:18002"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			s.register.cfg.Proxy = "group:" + group.ID
			email, password := "truck-09.linings@icloud.com", "saved-password"
			old := Account{AccessToken: "old-token", Email: &email, Password: &password, Status: "异常", MFAStatus: tc.mfa, SourceType: "web", Proxy: "direct"}
			if tc.missingPassword {
				old.Password = nil
			}
			if tc.normal {
				old.Status = "正常"
			}
			if tc.recovery {
				if err := s.register.recordRecoveryEmail(email, "账号已注册，密码和 2FA 待完成"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mfa == "enabled" {
				secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
				old.MFASecret = &secret
			}
			if err := s.store.SaveAccounts([]Account{old}); err != nil {
				t.Fatal(err)
			}
			codeRequests := 0
			var otpSentAt atomic.Int64
			mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/mailboxes/"+email+"/code" {
					t.Errorf("unexpected mailbox mutation or request: %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				if r.URL.Query().Get("after") == "" {
					t.Error("missing OTP freshness boundary")
				}
				boundary, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("after"))
				if err != nil || boundary.After(time.Unix(0, otpSentAt.Load()).Truncate(time.Second)) {
					t.Error("OTP sent during authorization or password submission was filtered out")
				}
				codeRequests++
				_, _ = io.WriteString(w, `{"success":true,"email":"truck-09.linings@icloud.com","code":"123456","message_id":"new-message"}`)
			}))
			defer mailAPI.Close()
			s.register.cfg.Mail = RegisterMailConfig{RequestTimeout: 2, WaitTimeout: 2, WaitInterval: 1, Providers: []map[string]any{{"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key"}}}
			registered, created, exchanged, enrolled := false, false, false, false
			passwordAdded := false
			s.register.passwordBrowserFactory = func(_ context.Context, _ *registerSession, account Account, add bool, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				passwordAdded = true
				if !add {
					t.Fatal("expected add-password branch")
				}
				stored := s.store.LoadAccounts()[0]
				if ptrString(account.Password) == "" || ptrString(stored.Password) != ptrString(account.Password) || stored.Status != "异常" {
					t.Fatal("password candidate must be persisted before password setup")
				}
				if tc.failAdd {
					return nil, errors.New("password setup failed")
				}
				return map[string]any{"access_token": account.AccessToken, "email": ptrString(account.Email)}, nil
			}
			factory := func(proxy string, _ int) (*registerSession, error) {
				if proxy != group.Nodes[0].URL {
					t.Errorf("repair login/MFA changed task proxy: %s", proxy)
				}
				jar, _ := cookiejar.New(nil)
				return &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
					code, body := 200, `{}`
					headers := make(http.Header)
					switch req.URL.Path {
					case "/backend-api/me":
						code, body = 401, `{"error":{"code":"token_expired"}}`
					case "/backend-api/accounts/add_password/eligibility":
						body = `{"eligible":true}`
					case "/api/accounts/authorize":
						if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) < 150*time.Second {
							t.Error("password repair still truncates configured mailbox polling to 90 seconds")
						}
						otpSentAt.Store(time.Now().UnixNano())
						code = 302
						headers.Set("Location", registerAuthBase+tc.landing)
					case "/email-verification", "/create-account/password", "/about-you":
						body = "<html></html>"
					case "/backend-api/sentinel/req":
						body = `{"token":"sentinel-token"}`
					case "/api/accounts/password/verify":
						t.Error("password verification requested from an email or signup authorization step")
						code = 400
					case "/api/accounts/user/register":
						otpSentAt.Store(time.Now().UnixNano())
						registered = true
						var input map[string]any
						if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input["password"] != password || input["username"] != email {
							t.Error("saved registration credentials not reused")
						}
						body = `{"page":{"type":"email_otp_send"},"continue_url":"https://auth.openai.com/api/accounts/email-otp/send"}`
					case "/api/accounts/email-otp/send", "/api/accounts/email-otp/resend":
					case "/api/accounts/email-otp/validate":
						body = `{"continue_url":"` + tc.continuation + `"}`
					case "/api/accounts/create_account":
						created = true
						body = `{"continue_url":"https://platform.openai.com/auth/callback?code=oauth-code"}`
					case "/api/accounts/oauth/token":
						exchanged = true
						body = `{"access_token":"new-token","refresh_token":"new-rt"}`
					case "/backend-api/accounts/mfa/enroll":
						enrolled = true
						current := s.store.LoadAccounts()[0]
						if current.AccessToken != "new-token" || current.Status != "异常" {
							t.Error("refreshed token must be saved before pending MFA is completed")
						}
						body = `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`
					case "/backend-api/accounts/mfa/user/activate_enrollment":
						body = `{"success":true}`
					default:
						t.Errorf("unexpected request: %s", req.URL.Path)
						code = 400
					}
					return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}}, nil
			}
			s.accountLoginSessionFactory = factory
			s.register.mfaSessionFactory = func(proxy string) (*registerSession, error) { return factory(proxy, 45) }
			err = s.repairAccountTarget(context.Background(), accountRepairTarget{account: old, email: email, pending: tc.normal}, 1)
			if tc.failAdd {
				current := s.store.LoadAccounts()[0]
				if err == nil || enrolled || current.Status != "异常" || current.PasswordStatus == "confirmed" || ptrString(current.Password) == "" || !s.register.hasRecoveryEmail(email) {
					t.Fatal("failed password setup lost the candidate or completed registration prematurely")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			current := s.store.LoadAccounts()[0]
			if current.PasswordStatus != "confirmed" || passwordAdded != (tc.landing == "/email-verification") {
				t.Fatalf("candidate password was not confirmed correctly: status=%s added=%t", current.PasswordStatus, passwordAdded)
			}
			if current.Proxy != "direct" {
				t.Fatal("repair persisted its temporary proxy over the account assignment")
			}
			if current.AccessToken != "new-token" || current.Status != "正常" || ptrString(current.Password) == "" || (!tc.missingPassword && ptrString(current.Password) != password) || codeRequests != 1 || !exchanged {
				t.Fatalf("repair incomplete: account=%+v codeRequests=%d exchanged=%t", current, codeRequests, exchanged)
			}
			wantMFA := tc.mfa == "pending" || tc.mfa == "failed" || (tc.recovery && tc.mfa != "enabled")
			if registered != (tc.landing == "/create-account/password") || created != isRegisterProfileURL(tc.continuation) || enrolled != wantMFA {
				t.Fatalf("wrong recovery steps: register=%t profile=%t MFA=%t", registered, created, enrolled)
			}
			if (wantMFA || tc.mfa == "enabled") && (current.MFAStatus != "enabled" || ptrString(current.MFASecret) == "") {
				t.Fatal("MFA was not persisted")
			}
			if tc.recovery && s.register.hasRecoveryEmail(email) {
				t.Fatal("completed registration retained its recovery record")
			}
		})
	}
}

func TestNormalReLoginDoesNotCreateAccountOnSignupLanding(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	email, password := "existing@icloud.com", "saved-password"
	s.accountLoginSessionFactory = func(string, int) (*registerSession, error) {
		jar, _ := cookiejar.New(nil)
		return &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			status := 200
			headers := make(http.Header)
			if req.URL.Path == "/api/accounts/authorize" {
				status = 302
				headers.Set("Location", registerAuthBase+"/create-account/password")
			} else if req.URL.Path != "/create-account/password" {
				t.Errorf("existing-account login attempted account creation: %s", req.URL.Path)
			}
			return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("<html></html>")), Request: req}, nil
		})}}, nil
	}
	_, err := s.reLoginAccountWithPasswordSecret(context.Background(), Account{Email: &email, Password: &password}, "")
	if err == nil || !strings.Contains(err.Error(), "signup path=/create-account/password") {
		t.Fatalf("unexpected result: %v", err)
	}
}

func TestCandidatePasswordFallbackDoesNotConfirmPassword(t *testing.T) {
	for _, failure := range []string{"invalid_password", "invalid_username_or_password", "invalid_state", "account_deactivated"} {
		t.Run(failure, func(t *testing.T) {
			jar, _ := cookiejar.New(nil)
			fallback, readCode := false, false
			session := &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{}`
				headers := make(http.Header)
				switch r.URL.Path {
				case "/api/accounts/authorize":
					status = 302
					headers.Set("Location", registerAuthBase+"/log-in/password")
				case "/log-in/password":
					body = "<html></html>"
				case "/backend-api/sentinel/req":
					body = `{"token":"sentinel-token"}`
				case "/api/accounts/password/verify":
					status = 401
					body = `{"error":{"code":"` + failure + `","message":"verification failed"}}`
				case "/api/accounts/passwordless/send-otp":
					fallback = true
					body = `{"page":{"type":"email_otp_verification"},"continue_url":"https://auth.openai.com/email-verification"}`
				case "/api/accounts/email-otp/send", "/api/accounts/email-otp/resend":
				case "/api/accounts/email-otp/validate":
					body = `{"continue_url":"https://platform.openai.com/auth/callback?code=otp-code"}`
				case "/api/accounts/oauth/token":
					body = `{"access_token":"otp-token"}`
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					status = 400
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}}
			_, err := session.loginWithPasswordContinuation(context.Background(), "partial@icloud.com", "candidate", "", &registerProfileConfig{}, func(context.Context, time.Time) (string, error) {
				readCode = true
				return "123456", nil
			})
			wantFallback := failure == "invalid_password" || failure == "invalid_username_or_password"
			if (err == nil) != wantFallback || fallback != wantFallback || readCode != wantFallback || session.passwordConfirmed {
				t.Fatalf("incorrect fallback or confirmation for %s: success=%t fallback=%t readCode=%t confirmed=%t", failure, err == nil, fallback, readCode, session.passwordConfirmed)
			}
		})
	}
}
