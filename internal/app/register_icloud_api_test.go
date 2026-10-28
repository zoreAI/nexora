package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
