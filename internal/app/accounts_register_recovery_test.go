package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func pendingRecoveryTestServer(t *testing.T) *Server {
	t.Helper()
	store := NewStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	svc := newRegisterService(t.TempDir(), store)
	s := &Server{store: store, register: svc, cfg: Config{AuthKey: "admin-key"}}
	svc.server = s
	return s
}

func TestPendingRegistrationsAppearWithoutEnteringServingPool(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	email := "imported@icloud.com"
	if err := s.store.SaveAccounts([]Account{{AccessToken: "existing", Email: &email, Status: "正常"}}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"partial@icloud.com", "imported@icloud.com"} {
		if err := s.register.savePendingPassword(email, "private-password", "注册接口已接受"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.register.recordRecoveryEmail("PARTIAL@icloud.com", "验证码超时"); err != nil {
		t.Fatal(err)
	}
	if err := s.register.recordRecoveryEmail("missing@icloud.com", "需要人工确认密码"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	s.handleAccounts(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var result struct {
		Items   []Account                `json:"items"`
		Pending []pendingRegisterAccount `json:"pending_registrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || len(result.Pending) != 2 || !result.Pending[0].PasswordSaved || result.Pending[0].State != "验证码超时" || result.Pending[1].PasswordSaved {
		t.Fatalf("unexpected response: %+v", result)
	}
	if strings.Contains(rec.Body.String(), "private-password") {
		t.Fatal("pending credentials leaked to listing")
	}
	if got := s.selectPendingRegisterEmails([]string{"partial@icloud.com", "PARTIAL@icloud.com", "imported@icloud.com", "unknown@icloud.com"}); len(got) != 1 {
		t.Fatalf("selected=%v", got)
	}
	if len(s.store.LoadAccounts()) != 1 {
		t.Fatal("tokenless registration entered serving pool")
	}
}

func TestImportedIncompleteRegistrationRemainsVisibleToRepairAfterRestart(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	partialEmail, completeEmail, legacyEmail, bannedEmail := "partial@example.com", "complete@example.com", "legacy@example.com", "banned@example.com"
	password, secret := "stored-password", "stored-secret"
	for _, email := range []string{partialEmail, completeEmail, bannedEmail} {
		if err := s.register.recordRecoveryEmail(email, "密码和 2FA 待完成"); err != nil {
			t.Fatal(err)
		}
	}
	accounts := []Account{
		{AccessToken: "partial-token", Email: &partialEmail, Status: "正常", Password: &password, PasswordStatus: "unverified", MFAStatus: "enabled", MFASecret: &secret},
		{AccessToken: "complete-token", Email: &completeEmail, Status: "正常", Password: &password, PasswordStatus: "confirmed", MFAStatus: "enabled", MFASecret: &secret},
		{AccessToken: "legacy-token", Email: &legacyEmail, Status: "正常"},
		{AccessToken: "banned-token", Email: &bannedEmail, Status: "封号"},
	}
	if err := s.store.SaveAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileRegisterRecoveryAccounts(); err != nil {
		t.Fatal(err)
	}
	reloaded := &Server{store: s.store, register: newRegisterService(t.TempDir(), s.store)}
	got := reloaded.store.LoadAccounts()
	if got[0].Status != "异常" || got[1].Status != "正常" || got[2].Status != "正常" || got[3].Status != "封号" {
		t.Fatal("incomplete import hidden or unrelated account changed")
	}
	targets := accountRepairTargets(got, nil)
	if len(targets) != 1 || targets[0].email != partialEmail || targets[0].pending {
		t.Fatal("pooled partial registration missing from repair queue")
	}
	if ptrString(got[0].Password) != password || ptrString(got[0].MFASecret) != secret || !reloaded.hasRegisterRecovery(got[0]) {
		t.Fatal("reconciling registration state lost credentials or recovery record")
	}
	// An unrelated edit must not put the same partial record back into service.
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/update", strings.NewReader(`{"access_token":"partial-token","status":"正常"}`))
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	s.handleAccountsUpdate(rec, req)
	if rec.Code != 200 || s.store.LoadAccounts()[0].Status != "异常" {
		t.Fatal("account edit bypassed unfinished registration state")
	}
}

func TestRecoveryWithEnabledMFAButMissingSecretRequiresManualKey(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	email, password := "partial@example.com", "accepted-password"
	a := Account{AccessToken: "token", Email: &email, Password: &password, PasswordStatus: "confirmed", Status: "异常", MFAStatus: "enabled"}
	if err := s.store.SaveAccounts([]Account{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.register.recordRecoveryEmail(email, "缺少 2FA 密钥"); err != nil {
		t.Fatal(err)
	}
	err := s.completeRecoveredAccount(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "手动补录") || s.store.LoadAccounts()[0].Status != "异常" || !s.register.hasRecoveryEmail(email) {
		t.Fatal("missing MFA secret was treated as complete")
	}
}

func TestPendingRegistrationRecoveryClassification(t *testing.T) {
	for _, tc := range []struct{ name, state, want string }{
		{"identity", "账号需要手动完成身份验证（/verify-your-identity），保留待修复记录", "identity_verification"},
		{"legacy identity response", `create account response missing continue_url body={"continue_url":"https://auth.openai.com/verify-your-identity"}`, "identity_verification"},
		{"identity after platform rejection", "ChatGPT Web 当前步骤 /verify-your-identity；此前 Platform 接续失败: code=registration_disallowed", "identity_verification"},
		{"web rejection", "ChatGPT Web 接续未完成: 步骤[create_profile] HTTP 400 code=registration_disallowed", "registration_disallowed"},
		{"platform rejection alone", "接续创建账号资料失败: code=registration_disallowed", "pending"},
		{"web otp after rejection", "ChatGPT Web 接续失败: 等待验证码超时；此前 Platform 接续失败: code=registration_disallowed", "pending"},
		{"previous identity", "ChatGPT Web 接续失败: 等待验证码超时；此前 Platform 接续失败: /verify-your-identity", "pending"},
		{"ordinary failure", "获取邮箱验证码失败: no_code", "pending"},
		{"empty", "", "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, hint := pendingRegisterRecoveryInfo(tc.state)
			if status != tc.want || hint == "" {
				t.Fatalf("got status=%q hint=%q, want=%q", status, hint, tc.want)
			}
		})
	}
}

func TestPendingRegistrationManualBlocksStayVisibleAndRequireExplicitRetry(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	states := map[string]string{
		"identity@icloud.com": "/verify-your-identity",
		"rejected@icloud.com": "ChatGPT Web HTTP 400 code=registration_disallowed",
		"retry@icloud.com":    "获取邮箱验证码失败: no_code",
	}
	for email, state := range states {
		if err := s.register.savePendingPassword(email, "saved-candidate", "密码待提交"); err != nil {
			t.Fatal(err)
		}
		if err := s.register.recordRecoveryEmail(email, state); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.pendingRegisterAccounts(); len(got) != 3 {
		t.Fatalf("blocked records hidden: %d", len(got))
	}
	emails := []string{"identity@icloud.com", "rejected@icloud.com", "retry@icloud.com", "RETRY@icloud.com"}
	if got := s.selectPendingRegisterEmails(emails); len(got) != 1 || got[0] != "retry@icloud.com" {
		t.Fatalf("batch includes manual blocks: %v", got)
	}
	if got := s.selectPendingRegisterEmails(emails, true); len(got) != 3 {
		t.Fatalf("explicit retry unavailable: %v", got)
	}
	for _, tc := range []struct {
		emails  []string
		retry   bool
		status  int
		skipped int
	}{
		{[]string{"identity@icloud.com"}, false, 400, 0},
		{[]string{"identity@icloud.com"}, true, 202, 0},
		{emails, false, 202, 2},
	} {
		body, _ := json.Marshal(map[string]any{"recovery_emails": tc.emails, "retry_blocked": tc.retry})
		req := httptest.NewRequest("POST", "/api/accounts/repair/start", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		s.handleAccountsRepairStart(rec, req)
		if tc.status == 400 {
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), "需要人工处理") {
				t.Fatalf("batch did not report manual block: %d %s", rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != tc.status {
			t.Fatalf("explicit retry rejected: %d %s", rec.Code, rec.Body.String())
		}
		var started struct {
			ID      string `json:"progress_id"`
			Total   int    `json:"total"`
			Skipped int    `json:"skipped"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &started)
		if started.Total != 1 || started.Skipped != tc.skipped {
			t.Fatalf("incorrect batch selection: %+v", started)
		}
		deadline := time.Now().Add(time.Second)
		for {
			progress, _ := s.accountRepairJobs.get(started.ID)
			if progress.Done {
				if progress.Total != 1 || progress.Processed != 1 || progress.Failed != 1 {
					t.Fatalf("unexpected retry result: %+v", progress)
				}
				break // no provider configured; the original record must survive
			}
			if time.Now().After(deadline) {
				t.Fatal("retry did not complete")
			}
			time.Sleep(time.Millisecond)
		}
	}
	s.register.cfg.Enabled = true
	s.register.cfg.Stats.JobKind = "repair_abnormal"
	s.register.runRepairAbnormalJob(context.Background())
	if cfg := s.register.snapshot(); cfg.Stats.Done != 1 || cfg.Stats.Fail != 1 || len(cfg.PendingPasswords) != 3 || len(cfg.RecoveryEmails) != 3 {
		t.Fatalf("batch retried manual blocks or lost credentials: %+v", cfg.Stats)
	}
}

func TestPendingMailboxSurvivesStateUpdatesAndRestart(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	if err := s.register.savePendingPassword("partial@icloud.com", "password", "提交待确认"); err != nil {
		t.Fatal(err)
	}
	mailbox := &registerMailbox{Address: "partial@icloud.com", Provider: "icloud_api", ProviderRef: "icloud_api#2", Extra: map[string]any{"mailbox_provider": "icloud_api"}}
	if err := s.register.savePendingMailbox(mailbox.Address, mailbox); err != nil {
		t.Fatal(err)
	}
	mailbox.Extra["mailbox_provider"] = "changed"
	if err := s.register.savePendingPassword(mailbox.Address, "password", "注册接口已接受"); err != nil {
		t.Fatal(err)
	}
	reloaded := newRegisterService(t.TempDir(), s.store)
	items := reloaded.snapshot().PendingPasswords
	if len(items) != 1 || items[0].Mailbox == nil || items[0].Mailbox.ProviderRef != "icloud_api#2" || items[0].Mailbox.Extra["mailbox_provider"] != "icloud_api" {
		t.Fatalf("mailbox context lost: %+v", items)
	}
}

func TestPendingRegistrationRecoveryStages(t *testing.T) {
	for _, scenario := range []struct{ landing, stage string }{
		{"login", "password-fails"}, {"login", "mfa-fails"}, {"login", "complete"},
		{"verification", "otp-fails"}, {"verification", "mfa-fails"}, {"verification", "complete"},
		{"verification", "callback-missing"},
		{"signup", "password-fails"}, {"signup", "mfa-fails"}, {"signup", "complete"},
		{"verification", "web-signup-complete"}, {"verification", "web-signup-fails"},
		{"verification", "existing-complete"}, {"verification", "existing-password-add-fails"},
		{"verification", "history-no-code"},
	} {
		t.Run(scenario.landing+"/"+scenario.stage, func(t *testing.T) {
			stage := scenario.stage
			landing := map[string]string{"login": "/log-in/password", "verification": "/email-verification", "signup": "/create-account/password"}[scenario.landing]
			passwordVerified, passwordRegistered, passwordResetUsed := false, false, false
			s := pendingRecoveryTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			codeRequests := 0
			mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/mailboxes/partial@icloud.com/code" {
					t.Errorf("recovery claimed or released a mailbox: %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				codeRequests++
				if r.URL.Query().Get("after") == "" {
					t.Error("missing code cutoff")
				}
				_, _ = io.WriteString(w, fmt.Sprintf(`{"success":true,"email":"partial@icloud.com","code":"123456","message_id":"mail-%d"}`, codeRequests))
			}))
			defer mailAPI.Close()
			s.register.cfg.Mail = RegisterMailConfig{RequestTimeout: 2, WaitTimeout: 2, WaitInterval: 1, Providers: []map[string]any{{"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key"}}}
			if err := s.register.savePendingPassword("partial@icloud.com", "saved-password", "注册接口已接受"); err != nil {
				t.Fatal(err)
			}
			if stage == "history-no-code" {
				if err := s.register.recordRecoveryEmail("partial@icloud.com", "接续注册未完成: 获取邮箱验证码失败: 最近结果：no_code: context deadline exceeded"); err != nil {
					t.Fatal(err)
				}
			}
			jar, _ := cookiejar.New(nil)
			profileCreated := false
			webSignupUsed := false
			s.register.signupBrowserFactory = func(_ context.Context, _ *registerSession, email, password string, _ registerProfileConfig, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				webSignupUsed = true
				if email != "partial@icloud.com" || password != "saved-password" {
					t.Fatal("Web signup did not reuse the original mailbox and candidate")
				}
				if stage == "web-signup-fails" {
					return nil, errors.New("ChatGPT Web signup failed")
				}
				return map[string]any{"access_token": "recovered-token", "email": email, "password_confirmed": true}, nil
			}
			// Keep the invalid-candidate branch deterministic. Production uses
			// the real browser reset flow; this fixture only verifies that a
			// failed saved password stays recoverable instead of being trusted.
			s.register.passwordResetBrowserFactory = func(_ context.Context, _ registerProvider, _ *registerMailbox, _ string, _ string) (map[string]any, error) {
				passwordResetUsed = true
				if stage == "password-fails" {
					return nil, errors.New("password reset failed")
				}
				return map[string]any{"access_token": "recovered-token", "email": "partial@icloud.com"}, nil
			}
			s.register.passwordBrowserFactory = func(_ context.Context, _ *registerSession, account Account, add bool, _ func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				if !add || account.PasswordStatus == "confirmed" {
					t.Fatal("email-only continuation must add and confirm its candidate password")
				}
				if stage == "existing-password-add-fails" {
					return nil, errors.New("password setup failed")
				}
				return map[string]any{"access_token": account.AccessToken, "email": ptrString(account.Email)}, nil
			}
			s.register.recoverySessionFactory = func(string) (*registerSession, error) {
				return &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
					code, body := 200, `{}`
					headers := make(http.Header)
					switch req.URL.Path {
					case "/backend-api/accounts/add_password/eligibility":
						body = `{"eligible":true}`
					case "/api/accounts/authorize":
						code = 302
						headers.Set("Location", registerAuthBase+landing)
					case "/log-in/password", "/email-verification", "/create-account/password", "/about-you":
						body = "<html></html>"
					case "/backend-api/sentinel/req":
						body = `{"token":"sentinel-token"}`
					case "/api/accounts/password/verify":
						passwordVerified = true
						if scenario.landing != "login" {
							t.Error("password verify requested from wrong authorization step")
						}
						if stage == "password-fails" {
							code = 400
							body = `{"error":{"code":"invalid_username_or_password","message":"saved-password invalid"}}`
						} else {
							body = `{"page":{"type":"email_otp_verification"}}`
						}
					case "/api/accounts/passwordless/send-otp":
						if stage == "password-fails" {
							code = 400
							body = `{"error":{"code":"invalid_username_or_password","message":"saved-password invalid"}}`
						}
					case "/api/accounts/user/register":
						passwordRegistered = true
						if scenario.landing != "signup" {
							t.Error("password registration requested from wrong step")
						}
						var input map[string]any
						if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input["username"] != "partial@icloud.com" || input["password"] != "saved-password" {
							t.Error("did not reuse persisted registration credentials")
						}
						if stage == "password-fails" {
							code = 400
							body = `{"error":{"code":"invalid_username_or_password","message":"saved-password invalid"}}`
						} else {
							body = `{"page":{"type":"email_otp_send"},"continue_url":"https://auth.openai.com/api/accounts/email-otp/send"}`
						}
					case "/api/accounts/email-otp/send":
						if req.Method != "GET" {
							t.Error("signup OTP request should follow supplied endpoint")
						}
					case "/api/accounts/email-otp/resend":
					case "/api/accounts/email-otp/validate":
						body = `{"continue_url":"https://auth.openai.com/about-you"}`
						if strings.HasPrefix(stage, "existing-") {
							body = `{"continue_url":"https://platform.openai.com/auth/callback?code=oauth-code"}`
						}
						if stage == "otp-fails" {
							code = 400
							body = `{"error":{"message":"invalid verification code"}}`
						}
					case "/api/accounts/create_account":
						profileCreated = true
						body = `{"continue_url":"https://platform.openai.com/auth/callback?code=oauth-code"}`
						if stage == "callback-missing" {
							body = `{"continue_url":"https://auth.openai.com/about-you"}`
						}
						if strings.HasPrefix(stage, "web-signup-") {
							code = 400
							body = `{"error":{"code":"registration_disallowed","message":"cannot create account"}}`
						}
					case "/api/accounts/oauth/token":
						body = `{"access_token":"recovered-token","refresh_token":"recovered-rt"}`
					case "/backend-api/accounts/mfa/enroll":
						accounts := s.store.LoadAccounts()
						if len(accounts) != 1 || accounts[0].Status != "异常" || ptrString(accounts[0].Password) != "saved-password" {
							t.Error("credentials were not saved before enrolling MFA")
						}
						if stage == "mfa-fails" {
							code = 500
						} else {
							body = `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"enroll-session"}`
						}
					case "/backend-api/accounts/mfa/user/activate_enrollment":
						body = `{"success":true}`
						cancel() // Avoid an unrelated live profile refresh after mocked activation.
					default:
						t.Errorf("unexpected upstream request: %s", req.URL.Path)
						code = 400
					}
					return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}}, nil
			}
			err := s.repairPendingRegistration(ctx, "partial@icloud.com")
			accounts := s.store.LoadAccounts()
			if stage == "web-signup-fails" && (err == nil || !strings.Contains(err.Error(), "code=registration_disallowed") || !strings.Contains(err.Error(), "ChatGPT Web signup failed")) {
				t.Fatal("Web fallback error lost the preceding Platform failure")
			}
			if webSignupUsed != (strings.HasPrefix(stage, "web-signup-") || stage == "callback-missing" || stage == "history-no-code") {
				t.Fatalf("Web signup fallback was not limited to the rejected Platform signup: used=%t err=%v", webSignupUsed, err)
			}
			if passwordVerified != (scenario.landing == "login") || passwordRegistered != (scenario.landing == "signup") {
				t.Fatalf("wrong branch: verify=%t register=%t", passwordVerified, passwordRegistered)
			}
			if passwordResetUsed != (stage == "password-fails") {
				t.Fatalf("saved-password reset branch used=%t stage=%s", passwordResetUsed, stage)
			}
			switch stage {
			case "history-no-code":
				if err != nil || codeRequests != 0 || profileCreated || len(accounts) != 1 || accounts[0].MFAStatus != "enabled" || accounts[0].PasswordStatus != "confirmed" || accounts[0].Status != "正常" || s.register.hasRecoveryEmail("partial@icloud.com") {
					t.Fatalf("known OTP failure did not recover through browser: %v", err)
				}
			case "password-fails", "otp-fails", "web-signup-fails":
				if err == nil || strings.Contains(err.Error(), "saved-password") || len(accounts) != 0 || s.register.pendingPasswordForEmail("partial@icloud.com") == "" || !s.register.hasRecoveryEmail("partial@icloud.com") {
					t.Fatalf("failed login lost recovery: %v", err)
				}
			case "mfa-fails":
				if err == nil || len(accounts) != 1 || accounts[0].Status != "异常" || accounts[0].MFAStatus != "failed" || !profileCreated || codeRequests != 1 {
					t.Fatalf("MFA failure state: err=%v accounts=%+v", err, accounts)
				}
			case "existing-password-add-fails":
				if err == nil || len(accounts) != 1 || accounts[0].Status != "异常" || accounts[0].PasswordStatus == "confirmed" || accounts[0].MFAStatus != "pending" || ptrString(accounts[0].Password) != "saved-password" || !s.register.hasRecoveryEmail("partial@icloud.com") || profileCreated {
					t.Fatal("existing account lost its recoverable credentials or was marked complete before password setup")
				}
			case "complete", "callback-missing", "web-signup-complete", "existing-complete":
				expectedCodeRequests := 1
				if stage == "callback-missing" {
					expectedCodeRequests = 2 // fresh authorization retries the OTP boundary once
				}
				if err != nil || len(accounts) != 1 || accounts[0].Status != "正常" || accounts[0].MFAStatus != "enabled" || ptrString(accounts[0].MFASecret) == "" || profileCreated != (stage != "existing-complete") || codeRequests != expectedCodeRequests || s.register.hasRecoveryEmail("partial@icloud.com") || s.register.pendingPasswordForEmail("partial@icloud.com") != "" {
					t.Fatalf("recovery incomplete: err=%v accounts=%+v", err, accounts)
				}
			}
		})
	}
}

func TestRepairStartAcceptsPendingRegistrationsAndKeepsFailedRecords(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	if err := s.register.recordRecoveryEmail("missing@icloud.com", "缺少密码"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/accounts/repair/start", strings.NewReader(`{"recovery_emails":["missing@icloud.com","MISSING@icloud.com","unknown@icloud.com"]}`))
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	s.handleAccountsRepairStart(rec, req)
	if rec.Code != 202 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID    string `json:"progress_id"`
		Total int    `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if started.Total != 1 {
		t.Fatalf("total=%d", started.Total)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		progress, _ := s.accountRepairJobs.get(started.ID)
		if progress.Done {
			if progress.Failed != 1 || progress.Processed != 1 || len(s.pendingRegisterAccounts()) != 1 {
				t.Fatalf("lost pending registration: %+v", progress)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("repair job did not finish")
}

func TestAbnormalRepairCompletesPendingMFAWithoutLoginCredentials(t *testing.T) {
	for _, activate := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete", false: "activation-fails"}[activate], func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			account := Account{AccessToken: "saved-token", Status: "异常", MFAStatus: "pending"}
			if err := s.store.SaveAccounts([]Account{account}); err != nil {
				t.Fatal(err)
			}
			s.register.mfaSessionFactory = func(string) (*registerSession, error) {
				return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
					body := `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`
					if strings.HasSuffix(req.URL.Path, "/activate_enrollment") {
						if activate {
							body = `{"success":true}`
						} else {
							body = `{"success":false}`
						}
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}}, nil
			}
			err := s.repairAbnormalAccount(context.Background(), account)
			current := s.store.LoadAccounts()[0]
			if activate {
				if err != nil || current.Status != "正常" || current.MFAStatus != "enabled" {
					t.Fatalf("MFA repair failed: %v %+v", err, current)
				}
			} else {
				if err == nil || current.Status != "异常" || current.MFAStatus != "pending" {
					t.Fatalf("failed MFA marked repaired: %v %+v", err, current)
				}
			}
		})
	}
}

func TestRegisterRepairJobIncludesTokenlessRecovery(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	if err := s.register.recordRecoveryEmail("missing@icloud.com", "缺少密码"); err != nil {
		t.Fatal(err)
	}
	s.register.cfg.Enabled = true
	s.register.cfg.Stats.JobKind = "repair_abnormal"
	s.register.runRepairAbnormalJob(context.Background())
	cfg := s.register.snapshot()
	if cfg.Enabled || cfg.Stats.Done != 1 || cfg.Stats.Fail != 1 || len(cfg.RecoveryEmails) != 1 || len(s.store.LoadAccounts()) != 0 {
		t.Fatalf("pending registration was skipped: %+v", cfg.Stats)
	}
}

func TestReLoginKeepsIncompleteMFAAbnormal(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	old := Account{AccessToken: "old-token", Status: "异常", MFAStatus: "failed"}
	if err := s.store.SaveAccounts([]Account{old}); err != nil {
		t.Fatal(err)
	}
	next := old
	next.AccessToken = "new-token"
	next.Status = "正常"
	if err := s.persistReLoginAccount(old.AccessToken, next); err != nil {
		t.Fatal(err)
	}
	current := s.store.LoadAccounts()[0]
	if current.AccessToken != "new-token" || current.Status != "异常" || current.MFAStatus != "failed" {
		t.Fatalf("login bypassed pending MFA: %+v", current)
	}
}
