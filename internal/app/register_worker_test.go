package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

type registerRedirectDoer struct {
	requests int
	landing  string
}

func (d *registerRedirectDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	d.requests++
	if req.URL.Path == "/api/accounts/authorize" {
		landing := d.landing
		if landing == "" {
			landing = "/create-account"
		}
		return &fhttp.Response{
			StatusCode: http.StatusFound,
			Header:     fhttp.Header{"Location": []string{landing}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	}
	return &fhttp.Response{
		StatusCode: http.StatusOK,
		Header:     fhttp.Header{},
		Body:       io.NopCloser(strings.NewReader("<html>create-account/password</html>")),
		Request:    req,
	}, nil
}

func TestPlatformAuthorizeFollowsCurlRedirect(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	doer := &registerRedirectDoer{}
	session.curl = doer
	if _, err := session.platformAuthorize(context.Background(), "device-id", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if doer.requests != 2 || session.lastAuthorizeLanding != "signup path=/create-account" {
		t.Fatalf("requests = %d, landing = %q", doer.requests, session.lastAuthorizeLanding)
	}
}

func TestPlatformAuthorizeStopsAtLoginLanding(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	doer := &registerRedirectDoer{landing: "/log-in/password"}
	session.curl = doer
	if _, err := session.platformAuthorize(context.Background(), "device-id", "test@example.com"); err == nil || !strings.Contains(err.Error(), "授权进入登录页") {
		t.Fatalf("authorize error = %v", err)
	}
	if doer.requests != 2 || session.lastAuthorizeLanding != "login path=/log-in/password" {
		t.Fatalf("requests = %d, landing = %q", doer.requests, session.lastAuthorizeLanding)
	}
}

func TestPlatformAuthorizeForRegistrationBranches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		landing    string
		wantResume bool
		wantCalls  int
	}{
		{name: "password signup", landing: "/create-account/password", wantCalls: 2},
		{name: "email verification", landing: "/email-verification", wantResume: true, wantCalls: 2},
		{name: "account choice", landing: "/log-in-or-create-account", wantResume: true, wantCalls: 2},
		{name: "login password", landing: "/log-in/password", wantResume: true, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := newRegisterSession("", 2, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.close()
			doer := &registerRedirectDoer{landing: tc.landing}
			session.curl = doer
			verifier, resume, err := session.platformAuthorizeForRegistration(context.Background(), "device-id", "test@example.com")
			if err != nil || verifier == "" || resume != tc.wantResume || doer.requests != tc.wantCalls {
				t.Fatalf("verifier present=%t resume=%t requests=%d err=%v", verifier != "", resume, doer.requests, err)
			}
		})
	}
}

func TestRegisterPasswordAfterVerificationLanding(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	var paths []string
	session.client.Transport = mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		body := `{"token":"test-token"}`
		switch req.URL.Path {
		case "/api/accounts/passwordless/send-otp":
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("send-otp content type = %q", got)
			}
			body = `{"page":{"type":"email_otp_verification"}}`
		case "/api/accounts/user/register":
			body = `{"page":{"type":"email_otp_send"},"continue_url":"https://auth.openai.com/api/accounts/email-otp/send"}`
		case "/api/accounts/email-otp/send":
			body = `{}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	if err := session.preparePasswordSignup(context.Background(), "device-id"); err != nil {
		t.Fatal(err)
	}
	if err := session.registerUser(context.Background(), "device-id", "test@example.com", "test-password"); err != nil {
		t.Fatal(err)
	}
	if err := session.sendOTP(context.Background(), "device-id"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/backend-api/sentinel/req", "/api/accounts/passwordless/send-otp", "/backend-api/sentinel/req", "/api/accounts/user/register", "/api/accounts/email-otp/send"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("request paths = %v, want %v", paths, want)
	}
}

func TestPreparePasswordSignupRejectsOtherStep(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	session.client.Transport = mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		body := `{"token":"test-token"}`
		if req.URL.Path == "/api/accounts/passwordless/send-otp" {
			body = `{"page":{"type":"login_password"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	if err := session.preparePasswordSignup(context.Background(), "device-id"); err == nil || !strings.Contains(err.Error(), "步骤不匹配") {
		t.Fatalf("prepare error = %v", err)
	}
}

func TestRegisterUserRequiresNextStepOnSuccess(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	session.client.Transport = mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		body := `{"token":"test-token"}`
		if req.URL.Path == "/api/accounts/user/register" {
			body = `{}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	if err := session.registerUser(context.Background(), "device-id", "test@example.com", "test-password"); err == nil || !strings.Contains(err.Error(), "缺少下一步页面") {
		t.Fatalf("register error = %v", err)
	}
}

func TestPlatformAuthorizeEmailOTPRequiresVerificationPage(t *testing.T) {
	for _, tc := range []struct {
		landing string
		wantErr bool
	}{
		{landing: "/email-verification"},
		{landing: "/log-in-or-create-account", wantErr: true},
	} {
		session, err := newRegisterSession("", 2, nil)
		if err != nil {
			t.Fatal(err)
		}
		doer := &registerRedirectDoer{landing: tc.landing}
		session.curl = doer
		_, err = session.platformAuthorizeEmailOTP(context.Background(), "device-id", "test@example.com")
		if (err != nil) != tc.wantErr || doer.requests != 2 {
			t.Errorf("landing=%s requests=%d err=%v", tc.landing, doer.requests, err)
		}
		session.close()
	}
}

func TestPlatformAuthorizeRetriesTransportEOF(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	requests := 0
	session.client.Transport = mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, io.EOF
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("<html>create-account/password</html>")),
			Request:    req,
		}, nil
	})
	if _, err := session.platformAuthorize(context.Background(), "device-id", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.HasPrefix(session.lastAuthorizeLanding, "signup") {
		t.Fatalf("requests = %d, landing = %q", requests, session.lastAuthorizeLanding)
	}
}

func TestPlatformAuthorizeDoesNotRetryHTTPError(t *testing.T) {
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	requests := 0
	session.client.Transport = mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_auth_step"}}`)),
			Request:    req,
		}, nil
	})
	if _, err := session.platformAuthorize(context.Background(), "device-id", "test@example.com"); err == nil {
		t.Fatal("expected authorization error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestAuthorizeLandingSummaryOmitsQuery(t *testing.T) {
	landing := authorizeLandingSummary("https://auth.openai.com/create-account?state=secret", "")
	if landing != "signup path=/create-account" {
		t.Fatalf("landing = %q", landing)
	}
	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/create-account", want: "signup"},
		{path: "/email-verification", want: "verification"},
		{path: "/log-in-or-create-account", want: "choice"},
	} {
		landing := authorizeLandingSummary("https://auth.openai.com"+tc.path, "password_verification")
		if landing != tc.want+" path="+tc.path {
			t.Fatalf("landing for %s = %q", tc.path, landing)
		}
	}
}

func TestRegisterUserResponseRejectsLoginLanding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		location string
		finalURL string
		body     string
	}{
		{name: "redirect", location: "/log-in/password"},
		{name: "followed redirect", finalURL: "https://auth.openai.com/log-in/password"},
		{name: "login body", body: `{"page":{"type":"password_verification"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.location != "" {
				resp.Header.Set("Location", tc.location)
			}
			if tc.finalURL != "" {
				req, err := http.NewRequest(http.MethodGet, tc.finalURL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp.Request = req
			}
			if err := registerUserResponseError(resp, tc.body); err == nil {
				t.Fatal("login landing was accepted as registration success")
			}
		})
	}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Location", "/email-verification")
	if err := registerUserResponseError(resp, ""); err != nil {
		t.Fatalf("email verification landing rejected: %v", err)
	}
}

func TestRegisterSessionKeepsFinalRedirectURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/log-in/password", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("login"))
	}))
	defer server.Close()
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	resp, err := session.do(context.Background(), http.MethodGet, server.URL+"/start", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL.Path != "/log-in/password" {
		t.Fatalf("final request URL = %v", resp.Request)
	}
}

func TestAuthorizeContinueUsesStructuredEmail(t *testing.T) {
	for _, pageType := range []string{"email_otp_verification", "password_verification"} {
		t.Run(pageType, func(t *testing.T) {
			session, err := newRegisterSession("", 2, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.close()
			var username struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			session.client.Transport = mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
				body := `{"token":"test"}`
				if req.URL.Path == "/api/accounts/authorize/continue" {
					var payload struct {
						Username json.RawMessage `json:"username"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(payload.Username, &username); err != nil {
						t.Fatal(err)
					}
					body = `{"page":{"type":"` + pageType + `"}}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			err = session.authorizeContinue(context.Background(), "device-id", "test@example.com")
			if username.Kind != "email" || username.Value != "test@example.com" {
				t.Fatalf("username = %+v", username)
			}
			if (err == nil) != (pageType == "email_otp_verification") {
				t.Fatalf("page type %s: err = %v", pageType, err)
			}
		})
	}
}

func TestRepairAbnormalDoesNotStartRegistrationWithoutAbnormalAccounts(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "正常"}}); err != nil {
		t.Fatal(err)
	}
	svc := newRegisterService(dir)
	svc.server = &Server{store: store}
	svc.cfg.Enabled = true
	svc.cfg.Stats = RegisterStats{JobKind: "repair_abnormal"}
	svc.run(context.Background())
	got := svc.snapshot()
	if got.Enabled || got.Stats.Done != 0 || got.Stats.Fail != 0 || got.Stats.FinishedAt == "" {
		t.Fatalf("unexpected repair result: %+v", got.Stats)
	}
	if len(store.LoadAccounts()) != 1 {
		t.Fatal("repair changed the account pool")
	}
}

func TestRepairAbnormalSkipsAccountWithoutLoginCredentials(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "异常"}}); err != nil {
		t.Fatal(err)
	}
	svc := newRegisterService(dir)
	svc.server = &Server{store: store}
	svc.cfg.Enabled = true
	svc.cfg.Stats = RegisterStats{JobKind: "repair_abnormal"}
	svc.run(context.Background())
	got := svc.snapshot()
	if got.Enabled || got.Stats.Done != 1 || got.Stats.Fail != 1 || got.Stats.Success != 0 || len(got.Failures) != 1 {
		t.Fatalf("unexpected repair result: %+v", got.Stats)
	}
	if accounts := store.LoadAccounts(); len(accounts) != 1 || accounts[0].Status != "异常" {
		t.Fatalf("repair changed accounts: %+v", accounts)
	}
}

func TestGenerateRegisterPKCE(t *testing.T) {
	verifier, challenge, err := generateRegisterPKCE()
	if err != nil {
		t.Fatalf("generateRegisterPKCE error: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatalf("empty verifier/challenge: %q %q", verifier, challenge)
	}
	if verifier == challenge {
		t.Fatalf("challenge should not equal verifier")
	}
}

func TestParseOutlookCredentialsDashFormat(t *testing.T) {
	items := parseOutlookCredentials("a@example.com----pass----client-id----refresh-token\n")
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	item := items[0]
	if item["email"] != "a@example.com" || item["password"] != "pass" || item["client_id"] != "client-id" || item["refresh_token"] != "refresh-token" {
		t.Fatalf("parsed item = %#v", item)
	}
}

func TestOutlookCredentialAvailableStaleInUse(t *testing.T) {
	fresh := map[string]any{"state": "in_use", "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if outlookCredentialAvailable(fresh) {
		t.Fatalf("fresh in_use should be unavailable")
	}
	stale := map[string]any{"state": "in_use", "updated_at": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)}
	if !outlookCredentialAvailable(stale) {
		t.Fatalf("stale in_use should be available")
	}
	used := map[string]any{"state": "used"}
	if outlookCredentialAvailable(used) {
		t.Fatalf("used should be unavailable")
	}
}

func TestNormalizeRegisterConfigKeepsProxyDefaultWhenMissing(t *testing.T) {
	cfg := normalizeRegisterConfigWithOptions(RegisterConfig{}, false)
	if !cfg.Mail.APIUseRegisterProxy {
		t.Fatalf("api_use_register_proxy default should stay true when missing")
	}
	cfg = normalizeRegisterConfigWithOptions(RegisterConfig{Mail: RegisterMailConfig{APIUseRegisterProxy: false}}, true)
	if cfg.Mail.APIUseRegisterProxy {
		t.Fatalf("api_use_register_proxy should allow explicit false")
	}
}

func TestRegisterProxyGroupRotatesBetweenTasks(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "register", Strategy: "round_robin", Nodes: []proxyNodeInput{
		{URL: "http://127.0.0.1:7891"},
		{URL: "http://127.0.0.1:7892"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &registerService{server: &Server{store: store}}
	for i, want := range []string{group.Nodes[0].URL, group.Nodes[1].URL, group.Nodes[0].URL} {
		route, err := svc.resolveRegisterProxy("group:" + group.ID)
		if err != nil || route.URL != want || route.NodeID == "" {
			t.Fatalf("selection %d: route = %+v, err = %v, want URL %q", i, route, err, want)
		}
	}
}

func TestMessageBeforeCodeBoundary(t *testing.T) {
	mailbox := &registerMailbox{CodeNotBefore: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	oldMessage := map[string]any{"received_at": "2026-07-16T11:59:40Z"}
	if !messageBeforeCodeBoundary(mailbox, oldMessage) {
		t.Fatalf("old message should be skipped")
	}
	newMessage := map[string]any{"received_at": "2026-07-16T12:00:02Z"}
	if messageBeforeCodeBoundary(mailbox, newMessage) {
		t.Fatalf("new message should not be skipped")
	}
	unknownTime := map[string]any{"subject": "code 123456"}
	if messageBeforeCodeBoundary(mailbox, unknownTime) {
		t.Fatalf("message without timestamp should not be skipped")
	}
}

func TestNormalizeGenericMailMessageExtractsBodyFields(t *testing.T) {
	message := normalizeGenericMailMessage(map[string]any{
		"subject":   "OpenAI",
		"body":      "Your code is 123456",
		"html_body": "<b>654321</b>",
	})
	if got := extractRegisterCode(message); got != "123456" {
		t.Fatalf("code = %q, want 123456; message=%#v", got, message)
	}
	htmlOnly := normalizeGenericMailMessage(map[string]any{"body_html": "<div>Use 234567</div>"})
	if got := extractRegisterCode(htmlOnly); got != "234567" {
		t.Fatalf("html code = %q, want 234567; message=%#v", got, htmlOnly)
	}
}

func TestExtractRegisterCodePrefersVerificationPhrase(t *testing.T) {
	message := map[string]any{
		"subject":      "Noise 111111",
		"text_content": "Your Verification code: 222222",
	}
	if got := extractRegisterCode(message); got != "222222" {
		t.Fatalf("code = %q, want 222222", got)
	}
	styled := map[string]any{"html_content": `<p style="background-color: #F3F3F3">333333</p><p>444444</p>`}
	if got := extractRegisterCode(styled); got != "333333" {
		t.Fatalf("styled code = %q, want 333333", got)
	}
	noise := map[string]any{"html_content": `<span style="color:#123456">&#177010;</span><b>555555</b>`}
	if got := extractRegisterCode(noise); got != "555555" {
		t.Fatalf("fallback code = %q, want 555555", got)
	}
	icloudRaw := map[string]any{
		"subject": "Your temporary OpenAI verification code",
		"text_content": strings.Join([]string{
			"X-Trace: abc-635375",
			"Authentication-Results: value 123456",
			"Enter this temporary verification code to continue:",
			"069912",
		}, "\n"),
	}
	if got := extractRegisterCode(icloudRaw); got != "069912" {
		t.Fatalf("icloud raw code = %q, want 069912", got)
	}
}

func TestWaitForRegisterCodeTimeoutIncludesDiagnostics(t *testing.T) {
	ctx := context.Background()
	mailbox := &registerMailbox{}
	code, err := waitForRegisterCode(ctx, mailbox, 1, 1, func() ([]map[string]any, error) {
		return []map[string]any{{"subject": "hello", "text_content": "no code"}}, nil
	})
	if code != "" {
		t.Fatalf("code = %q, want empty", code)
	}
	if err == nil || !strings.Contains(err.Error(), "最近轮询邮件1封") || !strings.Contains(err.Error(), "有内容邮件1封") {
		t.Fatalf("timeout error missing diagnostics: %v", err)
	}
}

func TestWrongEmailOTPCodeDetection(t *testing.T) {
	if !isWrongEmailOTPCode(errors.New(`{"code":"wrong_email_otp_code","message":"Wrong code. Please check it and try again."}`)) {
		t.Fatalf("wrong_email_otp_code should be detected")
	}
	if isWrongEmailOTPCode(errors.New("network error")) {
		t.Fatalf("network error should not be detected as wrong otp")
	}
}

func TestRegisterHTTPErrorAddsActionableDiagnostics(t *testing.T) {
	cfErr := registerHTTPError("GET", "https://auth.openai.com/test", 403, "<title>Just a moment...</title><script>cf-chl-test</script>")
	if !strings.Contains(cfErr.Error(), "被 Cloudflare 拦截") {
		t.Fatalf("cloudflare diagnostic missing: %v", cfErr)
	}

	domainErr := registerHTTPError("POST", "https://auth.openai.com/test", 400, `{"message":"Failed to create account. Please try again."}`)
	if !strings.Contains(domainErr.Error(), "邮箱域名很可能因滥用被封禁") {
		t.Fatalf("domain diagnostic missing: %v", domainErr)
	}
}

func TestShouldDisableRegisterCurlImpersonate(t *testing.T) {
	if !shouldDisableRegisterCurlImpersonate(errors.New("Abort trap: 6")) {
		t.Fatal("abort trap should disable curl-impersonate")
	}
	if !shouldDisableRegisterCurlImpersonate(errors.New("missing libidn2.0.dylib")) {
		t.Fatal("missing libidn2 should disable curl-impersonate")
	}
	if shouldDisableRegisterCurlImpersonate(errors.New("network timeout")) {
		t.Fatal("network timeout alone should not disable curl-impersonate")
	}
}

func TestRegisterEventsAcceptsQueryTokenForEventSource(t *testing.T) {
	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	server := &Server{cfg: Config{AuthKey: "admin-key"}, store: NewStore(t.TempDir()), register: svc}
	svc.server = server

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleRegisterEvents))
	defer httpServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/register/events?token=admin-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("content-type = %q, want text/event-stream", got)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: ") {
		resp.Body.Close()
		t.Fatalf("SSE initial frame = %q, err=%v", line, err)
	}
	cancel()
	_ = resp.Body.Close()
}

func TestRegisterEventsRejectsBadQueryToken(t *testing.T) {
	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	server := &Server{cfg: Config{AuthKey: "admin-key"}, store: NewStore(t.TempDir()), register: svc}
	svc.server = server

	req := httptest.NewRequest(http.MethodGet, "/api/register/events?token=bad", nil)
	rec := httptest.NewRecorder()
	server.handleRegisterEvents(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
}

func TestRegisterStartClearsOldLogs(t *testing.T) {
	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	svc.logs = []registerLogItem{{Time: nowISO(), Text: "old", Level: "yellow"}}
	svc.mu.Lock()
	svc.clearLogsLocked()
	svc.mu.Unlock()
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if len(svc.logs) != 0 {
		t.Fatalf("logs len = %d, want 0 after clear", len(svc.logs))
	}
}

func TestRegisterLoadKeepsEnabledState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "register.json")
	raw := `{"enabled":true,"total":3,"threads":2,"mail":{"providers":[]}}`
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatalf("write register config: %v", err)
	}
	svc := &registerService{path: path}
	if err := svc.load(); err != nil {
		t.Fatalf("load error: %v", err)
	}
	if !svc.snapshot().Enabled {
		t.Fatalf("enabled should stay true after load")
	}
	if svc.snapshot().Threads != 2 {
		t.Fatalf("threads = %d, want 2", svc.snapshot().Threads)
	}
}

func TestMaybeStartRegisterAutoStartsEnabledConfig(t *testing.T) {
	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	svc.cfg.Enabled = true
	started := make(chan struct{}, 1)
	svc.runFunc = func(context.Context) {
		started <- struct{}{}
	}
	server := &Server{register: svc}
	server.maybeStartRegister()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("auto start was not triggered")
	}
}

func TestRegisterStartIsIdempotentWhenRunning(t *testing.T) {
	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	svc.logs = []registerLogItem{{Time: nowISO(), Text: "old", Level: "yellow"}}
	svc.cancel = func() {}
	svc.cfg.Stats.JobID = "job-1"
	svc.cfg.Stats.Success = 7

	snapshot := svc.start("register")
	if snapshot.Stats.JobID != "job-1" {
		t.Fatalf("job_id = %q, want old job id preserved", snapshot.Stats.JobID)
	}
	if len(snapshot.Logs) != 1 || snapshot.Logs[0].Text != "old" {
		t.Fatalf("logs should stay untouched when already running: %#v", snapshot.Logs)
	}
}

func TestCreateProviderSupportsRegisterUIProviderTypes(t *testing.T) {
	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	types := []string{
		"cloudflare_temp_email",
		"ddg_mail",
		"tempmail_lol",
		"moemail",
		"inbucket",
		"duckmail",
		"gptmail",
		"yyds_mail",
		"cloudmail",
		"icloud_api",
		"outlook_token",
	}
	for _, providerType := range types {
		provider, err := svc.createProvider(RegisterMailConfig{
			RequestTimeout:      1,
			WaitTimeout:         1,
			WaitInterval:        1,
			APIUseRegisterProxy: true,
			Providers:           []map[string]any{{"type": providerType, "enable": true}},
		})
		if err != nil {
			t.Fatalf("createProvider(%s) error: %v", providerType, err)
		}
		provider.Close()
	}
}

func TestRandomBirthdateAgeRange(t *testing.T) {
	now := time.Now().UTC()
	for i := 0; i < 100; i++ {
		birthdate := randomBirthdate()
		parsed, err := time.Parse("2006-01-02", birthdate)
		if err != nil {
			t.Fatalf("birthdate %q parse error: %v", birthdate, err)
		}
		age := now.Year() - parsed.Year()
		if now.YearDay() < parsed.YearDay() {
			age--
		}
		if age < 18 || age > 30 {
			t.Fatalf("birthdate %q age = %d, want 18..30", birthdate, age)
		}
	}
}

func TestRegisterSessionBuildsSOCKS5Transport(t *testing.T) {
	session, err := newRegisterSession("socks5h://user:pass@127.0.0.1:1080", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	transport, ok := session.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", session.client.Transport)
	}
	if transport.Proxy != nil || transport.DialContext == nil {
		t.Fatalf("SOCKS5 transport = proxy=%v dial_context=%v", transport.Proxy != nil, transport.DialContext != nil)
	}
}

func TestRegisterSessionRejectsUnsupportedProxyScheme(t *testing.T) {
	if _, err := newRegisterSession("ftp://proxy.example:21", 1, nil); err == nil {
		t.Fatal("accepted unsupported registration proxy scheme")
	}
}

func TestDefaultRegisterProfileNameUsesLettersAndSpaces(t *testing.T) {
	for i := 0; i < 50; i++ {
		name := (registerProfileConfig{}).name()
		if !regexp.MustCompile(`^[A-Za-z ]+$`).MatchString(name) {
			t.Fatalf("name = %q, want letters and spaces only", name)
		}
	}
}

func TestMessageMatchesEmailRequiresRecipientMatch(t *testing.T) {
	msg := normalizeGenericMailMessage(map[string]any{
		"raw": "From: OpenAI <noreply@tm.openai.com>\r\nTo: alias-one@icloud.com\r\n\r\nYour code is 123456",
	})
	if !messageMatchesEmail(msg, "alias-one@icloud.com") {
		t.Fatal("expected recipient match")
	}
	if messageMatchesEmail(msg, "alias-two@icloud.com") {
		t.Fatal("unexpected match for a different alias")
	}
}

func TestRegisterCapacityExhaustedError(t *testing.T) {
	plainErr := errors.New("邮箱池暂无可用")
	if !isRegisterCapacityExhaustedError(plainErr) {
		t.Fatal("expected mailbox pool exhausted error")
	}
	if shouldStopForRegisterCapacityExhausted(plainErr) {
		t.Fatal("plain provider error should not stop the whole register job")
	}
	typedErr := registerCapacityExhaustedError{message: plainErr.Error()}
	if !shouldStopForRegisterCapacityExhausted(typedErr) {
		t.Fatal("typed capacity exhausted error should stop the register job")
	}
	if isRegisterCapacityExhaustedError(errors.New("POST https://auth.openai.com failed")) {
		t.Fatal("unexpected capacity exhausted match")
	}
}

func TestRegisterCapacityExhaustedDoesNotCountAsFailure(t *testing.T) {
	dir := t.TempDir()
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"code":"no_available_mailbox","message":"没有可用隐私邮箱"}`)
	}))
	defer mailAPI.Close()
	svc := newRegisterService(dir)
	svc.passwordStepProbe = func(context.Context, RegisterConfig) error { return nil }
	svc.mu.Lock()
	svc.cfg.Enabled = true
	svc.cfg.Mode = "total"
	svc.cfg.Total = 10
	svc.cfg.Threads = 1
	svc.cfg.CheckInterval = 1
	svc.cfg.Mail = RegisterMailConfig{
		RequestTimeout: 1,
		WaitTimeout:    1,
		WaitInterval:   1,
		Providers: []map[string]any{{
			"type":     "icloud_api",
			"enable":   true,
			"api_base": mailAPI.URL,
			"api_key":  "test-key",
		}},
	}
	svc.mu.Unlock()

	svc.runRegisterJob(context.Background())

	snapshot := svc.snapshot()
	if snapshot.Stats.Done != 0 || snapshot.Stats.Success != 0 || snapshot.Stats.Fail != 0 {
		t.Fatalf("stats = done:%d success:%d fail:%d, want all zero", snapshot.Stats.Done, snapshot.Stats.Success, snapshot.Stats.Fail)
	}
	if len(snapshot.Failures) != 0 {
		t.Fatalf("capacity exhaustion recorded as failure: %#v", snapshot.Failures)
	}
	if snapshot.Enabled {
		t.Fatal("register job should stop when capacity is exhausted")
	}
	if len(snapshot.Logs) == 0 || !strings.Contains(snapshot.Logs[len(snapshot.Logs)-1].Text, "成功0，失败0") {
		t.Fatalf("finish log should not count capacity exhaustion as failure: %#v", snapshot.Logs)
	}
}

func TestRegisterStopsBeforeClaimWhenPasswordStepUnavailable(t *testing.T) {
	claims := 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mailAPI.Close()
	svc := newRegisterService(t.TempDir())
	svc.passwordStepProbe = func(context.Context, RegisterConfig) error {
		return registerPasswordStepUnavailableError{message: "上游未进入设置密码页"}
	}
	svc.mu.Lock()
	svc.cfg.Enabled = true
	svc.cfg.Total = 1
	svc.cfg.Threads = 2
	svc.cfg.Mail.Providers = []map[string]any{{"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key"}}
	svc.mu.Unlock()
	svc.runRegisterJob(context.Background())
	snapshot := svc.snapshot()
	if claims != 0 || snapshot.Enabled || snapshot.Stats.Done != 0 || snapshot.Stats.Success != 0 || snapshot.Stats.Fail != 0 {
		t.Fatalf("claims=%d enabled=%t stats=%+v", claims, snapshot.Enabled, snapshot.Stats)
	}
	if len(snapshot.Logs) < 2 || !strings.Contains(snapshot.Logs[0].Text, "未进入设置密码页") {
		t.Fatalf("stop logs = %#v", snapshot.Logs)
	}
}

func TestRegisterFailurePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"code":"claim_rejected","message":"mailbox rejected"}`)
	}))
	defer mailAPI.Close()
	svc := newRegisterService(dir)
	svc.passwordStepProbe = func(context.Context, RegisterConfig) error { return nil }
	svc.mu.Lock()
	svc.cfg.Enabled = true
	svc.cfg.Mode = "total"
	svc.cfg.Total = 1
	svc.cfg.Threads = 1
	svc.cfg.Stats.JobID = "job-1"
	svc.cfg.Mail = RegisterMailConfig{
		RequestTimeout: 1,
		Providers:      []map[string]any{{"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key"}},
	}
	svc.mu.Unlock()
	svc.runRegisterJob(context.Background())

	reloaded := newRegisterService(dir).snapshot()
	if reloaded.Stats.Fail != 1 || len(reloaded.Failures) != 1 {
		t.Fatalf("failure not persisted: stats=%+v failures=%#v", reloaded.Stats, reloaded.Failures)
	}
	failure := reloaded.Failures[0]
	if failure.JobID != "job-1" || failure.Task != 1 || !strings.Contains(failure.Reason, "mailbox rejected") {
		t.Fatalf("unexpected failure: %#v", failure)
	}
	if len(reloaded.Logs) != 0 {
		t.Fatalf("live logs should remain in memory only: %#v", reloaded.Logs)
	}
}

func TestRegisterOpenAIErrorFields(t *testing.T) {
	code, message, typ := registerOpenAIErrorFields(`{"error":{"message":"Invalid authorization step.","type":"invalid_request_error","param":null,"code":"invalid_auth_step"}}`)
	if code != "invalid_auth_step" {
		t.Fatalf("code = %q", code)
	}
	if message != "Invalid authorization step." {
		t.Fatalf("message = %q", message)
	}
	if typ != "invalid_request_error" {
		t.Fatalf("type = %q", typ)
	}
}

func TestContinueURLFromCreateAccountResponse(t *testing.T) {
	noCodeReq, err := http.NewRequest(http.MethodPost, "https://platform.openai.com/create-account", nil)
	if err != nil {
		t.Fatalf("new request without code: %v", err)
	}
	noCodeResp := &http.Response{Request: noCodeReq}
	got, err := continueURLFromCreateAccountResponse(noCodeResp)
	if err != nil {
		t.Fatalf("no-code request response error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}

	reqURL, err := http.NewRequest(http.MethodPost, "https://platform.openai.com/auth/callback?code=req", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	reqResp := &http.Response{Request: reqURL}
	got, err = continueURLFromCreateAccountResponse(reqResp)
	if err != nil {
		t.Fatalf("request url response error: %v", err)
	}
	if got != "https://platform.openai.com/auth/callback?code=req" {
		t.Fatalf("got %q", got)
	}

	locResp := &http.Response{Header: http.Header{"Location": []string{"https://platform.openai.com/auth/callback?code=abc"}}}
	got, err = continueURLFromCreateAccountResponse(locResp)
	if err != nil {
		t.Fatalf("location response error: %v", err)
	}
	if got != "https://platform.openai.com/auth/callback?code=abc" {
		t.Fatalf("got %q", got)
	}

	jsonResp := &http.Response{
		Body:   io.NopCloser(strings.NewReader(`{"continue_url":"https://platform.openai.com/auth/callback?code=xyz"}`)),
		Header: http.Header{"Location": []string{"https://platform.openai.com/auth/callback?code=abc"}},
	}
	got, err = continueURLFromCreateAccountResponse(jsonResp)
	if err != nil {
		t.Fatalf("json response error: %v", err)
	}
	if got != "https://platform.openai.com/auth/callback?code=xyz" {
		t.Fatalf("got %q", got)
	}

	authErrPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"kind":"AuthApiFailure","errorCode":"registration_disallowed","requestId":"req-1"}`))
	authErrResp := &http.Response{Header: http.Header{"Location": []string{"https://auth.openai.com/error?payload=" + authErrPayload}}}
	got, err = continueURLFromCreateAccountResponse(authErrResp)
	if err == nil {
		t.Fatal("expected auth error from location payload")
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if !strings.Contains(err.Error(), "registration_disallowed") || !strings.Contains(err.Error(), "request_id=req-1") {
		t.Fatalf("unexpected auth error: %v", err)
	}
}

func TestRegisterHTTPError429OmitsBody(t *testing.T) {
	err := registerHTTPError("POST", "https://api.tempmail.lol/v2/inbox/create", 429, `{"error":"Rate limited (free)"}`)
	if got := err.Error(); got != "POST https://api.tempmail.lol/v2/inbox/create failed: status=429" {
		t.Fatalf("got %q", got)
	}
}

func TestGPTMailAuthHeaders(t *testing.T) {
	p := &gptMailRegisterProvider{entry: map[string]any{"api_key": "sk-test"}}
	headers := p.authHeaders()
	if headers["X-API-Key"] != "sk-test" {
		t.Fatalf("missing X-API-Key: %#v", headers)
	}
	if headers["Authorization"] != "Bearer sk-test" {
		t.Fatalf("missing Authorization bearer: %#v", headers)
	}
}
