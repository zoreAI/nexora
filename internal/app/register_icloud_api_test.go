package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestICloudAPIRegisterProviderReleasesFailedMailboxWithoutUnsupportedClaimFields(t *testing.T) {
	claimed := 0
	released := 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing global API key")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/mailboxes/claim":
			claimed++
			var body struct {
				Project string `json:"project"`
				Purpose string `json:"purpose"`
				Count   int    `json:"count"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Project != "openai" || body.Purpose != "register" || body.Count != 1 {
				t.Errorf("claim body = %+v", body)
			}
			if claimed == 1 {
				_, _ = w.Write([]byte(`{"success":true,"mailbox":{"email":"alias@icloud.com","api_active":true,"icloud_active":true}}`))
			} else {
				_, _ = w.Write([]byte(`{"success":false,"code":"no_available_mailbox"}`))
			}
		case "/api/v1/mailboxes/alias@icloud.com/release":
			released++
			_, _ = w.Write([]byte(`{"success":true,"mailbox":{"email":"alias@icloud.com"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer mailAPI.Close()
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := &registerService{}
	provider := &iCloudAPIRegisterProvider{registerProviderBase: registerProviderBase{sess: session}, entry: map[string]any{"api_base": mailAPI.URL, "api_key": "test-key"}, service: svc}
	defer provider.Close()
	mailbox, err := provider.CreateMailbox("")
	if err != nil {
		t.Fatal(err)
	}
	provider.MarkFailed(mailbox, "registration failed")
	if released != 1 {
		t.Fatalf("released = %d", released)
	}
	if _, err := provider.CreateMailbox(""); err == nil {
		t.Fatal("expected exhausted pool")
	}
}

func TestICloudAPIRegisterProviderKeepsPendingRecoveryMailbox(t *testing.T) {
	released := 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		released++
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer mailAPI.Close()
	svc := &registerService{cfg: RegisterConfig{
		PendingPasswords: []registerPendingPassword{{Email: "alias@icloud.com", Password: "candidate"}},
	}}
	provider := &iCloudAPIRegisterProvider{entry: map[string]any{"api_base": mailAPI.URL, "api_key": "test-key"}, service: svc}
	provider.MarkFailed(&registerMailbox{Address: "alias@icloud.com"}, "browser proxy unsupported")
	if released != 0 {
		t.Fatalf("released pending mailbox %d times", released)
	}
}

func TestRegisterTaskStopsAfterClaimingMailboxWithPendingPassword(t *testing.T) {
	claims, releases := 0, 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/mailboxes/claim":
			claims++
			_, _ = w.Write([]byte(`{"success":true,"mailbox":{"email":"pending@icloud.com","api_active":true,"icloud_active":true}}`))
		case "/api/v1/mailboxes/pending@icloud.com/release":
			releases++
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected automatic recovery request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer mailAPI.Close()
	store := NewStore(t.TempDir())
	defer store.Close()
	cfg := RegisterConfig{Mail: RegisterMailConfig{RequestTimeout: 2, Providers: []map[string]any{{
		"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key",
	}}}}
	svc := &registerService{store: store, server: &Server{store: store}, cfg: cfg}
	if err := svc.savePendingPassword("pending@icloud.com", "saved-password", "注册接口已接受"); err != nil {
		t.Fatal(err)
	}
	ok, err := svc.runRegisterTask(context.Background(), cfg, 1)
	var recoveryErr registerRecoveryRequiredError
	if ok || !errors.As(err, &recoveryErr) || claims != 1 || releases != 0 {
		t.Fatalf("task result: ok=%t err=%v claims=%d releases=%d", ok, err, claims, releases)
	}
	if got := svc.pendingPasswordForEmail("pending@icloud.com"); got != "saved-password" {
		t.Fatal("saved password was lost")
	}
	if !svc.hasRecoveryEmail("pending@icloud.com") {
		t.Fatal("claimed mailbox was not marked for manual repair")
	}
}

func TestRegisterJobSkipsPendingMailboxAndKeepsClaiming(t *testing.T) {
	claims, releases := 0, 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/mailboxes/claim":
			claims++
			if claims == 1 {
				_, _ = w.Write([]byte(`{"success":true,"mailbox":{"email":"pending@icloud.com","api_active":true,"icloud_active":true}}`))
			} else {
				_, _ = w.Write([]byte(`{"success":false,"code":"no_available_mailbox"}`))
			}
		case "/api/v1/mailboxes/pending@icloud.com/release":
			releases++
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer mailAPI.Close()
	store := NewStore(t.TempDir())
	defer store.Close()
	cfg := RegisterConfig{Enabled: true, Mode: "total", Total: 1, Threads: 1, Mail: RegisterMailConfig{
		RequestTimeout: 2, Providers: []map[string]any{{"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key"}},
	}}
	svc := &registerService{store: store, server: &Server{store: store}, cfg: cfg}
	if err := svc.savePendingPassword("pending@icloud.com", "saved-password", "注册接口已接受"); err != nil {
		t.Fatal(err)
	}
	svc.runRegisterJob(context.Background())
	snapshot := svc.snapshot()
	if claims != 2 || releases != 0 || snapshot.Stats.Done != 0 || snapshot.Stats.Fail != 0 || len(snapshot.Failures) != 0 {
		t.Fatalf("claims=%d releases=%d stats=%+v failures=%v", claims, releases, snapshot.Stats, snapshot.Failures)
	}
	for _, entry := range snapshot.Logs {
		if strings.Contains(entry.Text, "待接续邮箱需要处理，已暂停领取新邮箱") {
			t.Fatalf("pending mailbox stopped registration: %q", entry.Text)
		}
	}
}

func TestICloudAPIRegisterProviderClaimsAndReadsCode(t *testing.T) {
	codeRequests := 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/mailboxes/claim":
			if r.Method != http.MethodPost {
				t.Errorf("claim method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"success":true,"mailbox":{"email":"alias@icloud.com","api_active":true,"icloud_active":true}}`))
		case "/api/v1/mailboxes/alias@icloud.com/code":
			codeRequests++
			if r.URL.Query().Get("after") == "" || r.URL.Query().Get("keyword") != "OpenAI" {
				t.Errorf("code query = %q", r.URL.RawQuery)
			}
			if codeRequests == 1 {
				_, _ = w.Write([]byte(`{"success":false,"code":"no_code","retryable":true}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"success":true,"email":"alias@icloud.com","code":"123456","message_id":"msg-1","received_at":%q}`, time.Now().UTC().Format(time.RFC3339))
		default:
			t.Errorf("unexpected path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer mailAPI.Close()

	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &iCloudAPIRegisterProvider{
		registerProviderBase: registerProviderBase{sess: session},
		entry:                map[string]any{"api_base": mailAPI.URL + "/", "api_key": "test-key", "wait_timeout": 4, "wait_interval": 1},
	}
	defer provider.Close()
	mailbox, err := provider.CreateMailbox("")
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.Address != "alias@icloud.com" {
		t.Fatalf("claimed address = %q", mailbox.Address)
	}
	mailbox.CodeNotBefore = time.Now()
	code, err := provider.WaitForCode(context.Background(), mailbox)
	if err != nil || code != "123456" || codeRequests != 2 {
		t.Fatalf("code = %q, requests = %d, err = %v", code, codeRequests, err)
	}
}

func TestAccountEmailCodeReadsExistingICloudMailboxWithoutClaim(t *testing.T) {
	requests := 0
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/mailboxes/alias@icloud.com/code" {
			t.Errorf("unexpected mailbox request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Query().Get("after") == "" {
			t.Error("missing mailbox authorization or OTP cutoff")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"email":"alias@icloud.com","code":"123456"}`))
	}))
	defer mailAPI.Close()
	s := &Server{register: &registerService{cfg: RegisterConfig{Mail: RegisterMailConfig{
		RequestTimeout: 2,
		Providers:      []map[string]any{{"type": "icloud_api", "enable": true, "api_base": mailAPI.URL, "api_key": "test-key"}},
	}}}}
	session, err := newRegisterSession("", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	email := "alias@icloud.com"
	account := Account{Email: &email}
	if !s.canReadAccountEmailCode(account) {
		t.Fatal("configured iCloud API should support an account without mailbox metadata")
	}
	code, err := s.waitAccountEmailCode(context.Background(), session, account, email, "", time.Now().UTC())
	if err != nil || code != "123456" || requests != 1 {
		t.Fatalf("code = %q, requests = %d, err = %v", code, requests, err)
	}
}

func TestAccountEmailCodeRequiresConfiguredProvider(t *testing.T) {
	email := "alias@icloud.com"
	account := Account{Email: &email}
	if (&Server{}).canReadAccountEmailCode(account) {
		t.Fatal("iCloud address alone must not count as a configured code reader")
	}
	s := &Server{register: &registerService{cfg: RegisterConfig{Mail: RegisterMailConfig{
		Providers: []map[string]any{{"type": "icloud_api", "enable": false}},
	}}}}
	if s.canReadAccountEmailCode(account) {
		t.Fatal("disabled iCloud provider must not enable email login")
	}
}

func TestICloudAPIRegisterProviderRejectsMissingKeyAndLegacyType(t *testing.T) {
	session, err := newRegisterSession("", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &iCloudAPIRegisterProvider{registerProviderBase: registerProviderBase{sess: session}, entry: map[string]any{"api_base": "http://127.0.0.1:8787"}}
	defer provider.Close()
	if _, err := provider.CreateMailbox(""); err == nil || !strings.Contains(err.Error(), "全局 API Key") {
		t.Fatalf("missing API key error = %v", err)
	}
	provider.entry["api_base"] = "http://127.0.0.1:8787/api"
	provider.entry["api_key"] = "test-key"
	if _, err := provider.CreateMailbox(""); err == nil || !strings.Contains(err.Error(), "服务根地址") {
		t.Fatalf("invalid API base error = %v", err)
	}

	svc := &registerService{path: t.TempDir() + "/register.json", cfg: defaultRegisterConfig()}
	_, err = svc.createProvider(RegisterMailConfig{Providers: []map[string]any{{"type": "icloud_alias", "enable": true}}})
	if err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("legacy provider error = %v", err)
	}
}

func TestICloudAPIRegisterProviderReportsEmptyPool(t *testing.T) {
	mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"code":"no_available_mailbox"}`))
	}))
	defer mailAPI.Close()
	session, err := newRegisterSession("", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &iCloudAPIRegisterProvider{registerProviderBase: registerProviderBase{sess: session}, entry: map[string]any{"api_base": mailAPI.URL, "api_key": "test-key"}}
	defer provider.Close()
	_, err = provider.CreateMailbox("")
	var capacityErr registerCapacityExhaustedError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("empty pool error = %v, want capacity error", err)
	}
}

func TestICloudAPIRegisterProviderKeepsRegistrationProxyWhenMailIsDirect(t *testing.T) {
	svc := &registerService{
		path: t.TempDir() + "/register.json",
		cfg:  RegisterConfig{Proxy: "http://127.0.0.1:7897"},
	}
	provider, err := svc.createProvider(RegisterMailConfig{
		RequestTimeout: 1,
		Providers: []map[string]any{{
			"type": "icloud_api", "enable": true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	ic, ok := provider.(*iCloudAPIRegisterProvider)
	if !ok || ic.mailSess == nil {
		t.Fatalf("provider = %T, mail session missing", provider)
	}
	registerTransport := ic.sess.client.Transport.(*http.Transport)
	mailTransport := ic.mailSess.client.Transport.(*http.Transport)
	if registerTransport.Proxy == nil || mailTransport.Proxy != nil {
		t.Fatal("registration must use the configured proxy while mail API stays direct")
	}
}

func TestICloudAPIRegisterProviderUsesSelectedGroupNode(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "registration", Strategy: "round_robin", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7891"}}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &registerService{server: &Server{store: store}, cfg: RegisterConfig{Proxy: "group:" + group.ID}}
	route, err := svc.resolveRegisterProxy(svc.cfg.Proxy)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := svc.createProviderWithProxy(RegisterMailConfig{RequestTimeout: 1, Providers: []map[string]any{{"type": "icloud_api", "enable": true}}}, route.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	ic := provider.(*iCloudAPIRegisterProvider)
	registerTransport := ic.sess.client.Transport.(*http.Transport)
	mailTransport := ic.mailSess.client.Transport.(*http.Transport)
	req, err := http.NewRequest(http.MethodGet, "https://auth.openai.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := registerTransport.Proxy(req)
	if err != nil || selected == nil || selected.String() != route.URL || mailTransport.Proxy != nil {
		t.Fatalf("registration proxy selected = %v, mail direct = %t, err = %v", selected, mailTransport.Proxy == nil, err)
	}
}

func TestICloudCodeWaitBoundsBlockedRequestAndReportsAPIResults(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked=%t", blocked), func(t *testing.T) {
			mailAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("wait_ms") != "1000" {
					t.Error("mail server poll duration must be explicit")
				}
				if blocked {
					<-r.Context().Done()
					return
				}
				_, _ = w.Write([]byte(`{"success":false,"code":"no_code","retryable":true}`))
			}))
			defer mailAPI.Close()
			session, err := newRegisterSession("", 60, nil)
			if err != nil {
				t.Fatal(err)
			}
			provider := &iCloudAPIRegisterProvider{registerProviderBase: registerProviderBase{sess: session}, entry: map[string]any{
				"api_base": mailAPI.URL, "api_key": "test-key", "wait_timeout": 1,
			}}
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started := time.Now()
			_, err = provider.WaitForCode(ctx, &registerMailbox{Address: "alias@icloud.com"})
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(started) > 2500*time.Millisecond {
				t.Fatalf("per-round timeout failed to bound the request: %v", err)
			}
			if strings.Contains(err.Error(), "邮件0封") || !strings.Contains(err.Error(), "取码请求1次") {
				t.Fatalf("API failures were reported as mailbox message counts: %v", err)
			}
			if !blocked && !strings.Contains(err.Error(), "no_code") {
				t.Fatalf("missing provider result: %v", err)
			}
		})
	}
}
