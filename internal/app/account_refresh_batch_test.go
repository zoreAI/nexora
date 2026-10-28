package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

func TestAccountRefreshBatchBoundsConcurrency(t *testing.T) {
	accounts := make([]Account, 20)
	for i := range accounts {
		accounts[i].AccessToken = fmt.Sprintf("token-%d", i)
	}
	var active, maximum atomic.Int32
	results := runAccountRefreshBatch(context.Background(), accounts, func(_ context.Context, _ Account) error {
		current := active.Add(1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		time.Sleep(15 * time.Millisecond)
		active.Add(-1)
		return nil
	})
	if maximum.Load() < 2 || maximum.Load() > 8 {
		t.Fatalf("maximum concurrent refreshes = %d, want 2..8", maximum.Load())
	}
	for i, err := range results {
		if err != nil {
			t.Fatalf("account %d refresh: %v", i, err)
		}
	}
}

func TestAccountRefreshBatchHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := runAccountRefreshBatch(ctx, []Account{{AccessToken: "one"}}, func(context.Context, Account) error {
		t.Fatal("canceled batch sent an upstream request")
		return nil
	})
	if len(results) != 1 || !errors.Is(results[0], context.Canceled) {
		t.Fatalf("canceled batch results = %v", results)
	}
}

func TestAccountRefreshBatchAllowsEmailReloginTime(t *testing.T) {
	email := "alias@icloud.com"
	accounts := []Account{{AccessToken: "icloud", Email: &email}, {AccessToken: "token-only"}}
	results := runAccountRefreshBatch(context.Background(), accounts, func(ctx context.Context, account Account) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("refresh context has no deadline")
		}
		remaining := time.Until(deadline)
		if account.AccessToken == "icloud" && remaining < 2*time.Minute {
			return fmt.Errorf("iCloud OTP refresh only has %s", remaining)
		}
		if account.AccessToken == "token-only" && remaining > time.Minute {
			return fmt.Errorf("token-only refresh has %s", remaining)
		}
		return nil
	})
	for i, err := range results {
		if err != nil {
			t.Fatalf("account %d: %v", i, err)
		}
	}
}

func TestAccountRefreshSlotSharedAndCancelable(t *testing.T) {
	s := &Server{accountPool: &accountPool{refreshSlots: make(chan struct{}, 1)}}
	release, err := s.acquireAccountRefreshSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.acquireAccountRefreshSlot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting for occupied slot: %v", err)
	}
	release()
	releaseNext, err := s.acquireAccountRefreshSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseNext()
}

func TestLimitedWatcherRefreshPreservesConcurrentChanges(t *testing.T) {
	store := NewStore(t.TempDir())
	limitedAt := "2026-10-16T00:00:00Z"
	observed := Account{AccessToken: "token", Type: "Plus", Status: "限流", Fail: 1, RateLimitedAt: &limitedAt}
	if err := store.SaveAccounts([]Account{observed}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	if err := store.UpdateAccounts(func(accounts []Account) []Account {
		accounts[0].Success = 3
		return accounts
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.applyLimitedAccountRefresh(observed, Account{Status: "正常", Type: "free", Quota: 4}); err != nil {
		t.Fatal(err)
	}
	account := store.LoadAccounts()[0]
	if account.Status != "正常" || account.Type != "free" || account.Quota != 4 || account.Success != 3 || account.RateLimitedAt != nil {
		t.Fatalf("confirmed quota refresh = %+v", account)
	}
	if err := store.UpdateAccounts(func(accounts []Account) []Account {
		accounts[0].Status = "限流"
		accounts[0].Fail = 2
		return accounts
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.applyLimitedAccountRefresh(observed, Account{Status: "正常", Quota: 7}); err != nil {
		t.Fatal(err)
	}
	account = store.LoadAccounts()[0]
	if account.Status != "限流" || account.Fail != 2 || account.Quota != 4 {
		t.Fatalf("stale watcher result changed account = %+v", account)
	}
}

func TestAccountInfoRefreshDoesNotOverwriteNewerUsage(t *testing.T) {
	store := NewStore(t.TempDir())
	observed := Account{AccessToken: "token", Status: "正常", Quota: 5}
	if err := store.SaveAccounts([]Account{observed}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	if err := store.UpdateAccounts(func(accounts []Account) []Account {
		accounts[0].Fail++
		accounts[0].Status = "限流"
		accounts[0].Quota = 0
		return accounts
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.applyAccountInfoRefresh(observed, Account{Status: "正常", Quota: 5}); err == nil {
		t.Fatal("stale account refresh unexpectedly succeeded")
	}
	account := store.LoadAccounts()[0]
	if account.Status != "限流" || account.Quota != 0 || account.Fail != 1 {
		t.Fatalf("stale refresh changed newer account state = %+v", account)
	}
}

func TestAccountRefreshAuthFailureRequiresSameCredentials(t *testing.T) {
	store := NewStore(t.TempDir())
	refresh := "old-refresh"
	observed := Account{AccessToken: "access-token", RefreshToken: &refresh, Status: "正常", Quota: 5}
	if err := store.SaveAccounts([]Account{observed}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	if err := store.UpdateAccounts(func(accounts []Account) []Account {
		next := "new-refresh"
		accounts[0].RefreshToken = &next
		return accounts
	}); err != nil {
		t.Fatal(err)
	}
	s.markAccountRefreshAuthFailure(observed, errors.New("GET /backend-api/me failed: status=401"))
	account := store.LoadAccounts()[0]
	if account.Status != "正常" || account.Fail != 0 || account.Quota != 5 {
		t.Fatalf("stale 401 invalidated refreshed account: %+v", account)
	}
}

func TestUnknownQuotaDoesNotReleaseLimitedAccount(t *testing.T) {
	limitedAt := "2026-10-16T00:00:00Z"
	account := Account{AccessToken: "token", Type: "Plus", Status: "限流", RateLimitedAt: &limitedAt}
	mergeRefreshedAccount(&account, Account{Status: "正常", ImageQuotaUnknown: true})
	if account.Status != "限流" || account.Type != "Plus" || account.RateLimitedAt == nil {
		t.Fatalf("unknown quota refresh changed limited state = %+v", account)
	}
}

func TestGetUserInfoRequiresAllAccountChecks(t *testing.T) {
	client := &UpstreamClient{token: "token", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/me":
			return upstreamTestResponse(200, `{"email":"user@example.com"}`), nil
		case "/backend-api/conversation/init":
			return upstreamTestResponse(503, "unavailable"), nil
		case "/backend-api/accounts/check/v4-2023-04-27":
			return upstreamTestResponse(200, `{"accounts":{"default":{"account":{"plan_type":"plus"}}}}`), nil
		default:
			t.Fatalf("unexpected account info path %s", req.URL.Path)
			return nil, nil
		}
	})}
	if _, err := client.GetUserInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "conversation/init") {
		t.Fatalf("missing quota request should fail refresh: %v", err)
	}
}

func TestGetUserInfoPrefersAuthenticationFailure(t *testing.T) {
	client := &UpstreamClient{token: "token", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/me":
			return upstreamTestResponse(200, `{"email":"user@example.com"}`), nil
		case "/backend-api/conversation/init":
			return upstreamTestResponse(503, "unavailable"), nil
		case "/backend-api/accounts/check/v4-2023-04-27":
			return upstreamTestResponse(401, `{"error":"unauthorized"}`), nil
		default:
			t.Fatalf("unexpected account info path %s", req.URL.Path)
			return nil, nil
		}
	})}
	if _, err := client.GetUserInfo(context.Background()); !isInvalidTokenErrorText(err) {
		t.Fatalf("authentication failure should take precedence: %v", err)
	}
}

func TestGetUserInfoKeepsUnknownQuotaWhenChecksSucceed(t *testing.T) {
	client := &UpstreamClient{token: "token", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/me":
			return upstreamTestResponse(200, `{"email":"user@example.com"}`), nil
		case "/backend-api/conversation/init":
			return upstreamTestResponse(200, `{"limits_progress":[]}`), nil
		case "/backend-api/accounts/check/v4-2023-04-27":
			return upstreamTestResponse(200, `{"accounts":{"default":{"account":{}}}}`), nil
		default:
			t.Fatalf("unexpected account info path %s", req.URL.Path)
			return nil, nil
		}
	})}
	info, err := client.GetUserInfo(context.Background())
	if err != nil || info.Type != "" || !info.ImageQuotaUnknown {
		t.Fatalf("account info type=%q unknown=%t err=%v", info.Type, info.ImageQuotaUnknown, err)
	}
}

func TestParseAccountTrial(t *testing.T) {
	tests := []struct {
		name, payload, state, promoID, label string
	}{
		{"free trial", `{"account":{"plan_type":"free"},"entitlement":{"subscription_plan":"chatgptfreeplan"},"eligible_promo_campaigns":{"plus":{"id":"plus-1-month-free"}}}`, "eligible", "plus-1-month-free", "一个月免费"},
		{"discount", `{"account":{"plan_type":"free"},"eligible_promo_campaigns":{"plus":{"id":"half-off","metadata":{"discount":{"percentage":50},"duration":{"num_periods":2}}}}}`, "discount", "half-off", "2个月5折"},
		{"subscribed", `{"account":{"plan_type":"plus"},"eligible_promo_campaigns":{"plus":{"id":"plus-1-month-free"}}}`, "subscribed", "", "已订阅"},
		{"no campaign", `{"account":{"plan_type":"free"}}`, "not-eligible", "", "无优惠"},
		{"missing account", `{"eligible_promo_campaigns":{}}`, "unknown", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entry map[string]any
			if err := json.Unmarshal([]byte(tt.payload), &entry); err != nil {
				t.Fatal(err)
			}
			state, promoID, label := parseAccountTrial(entry)
			if state != tt.state || promoID != tt.promoID || label != tt.label {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", state, promoID, label, tt.state, tt.promoID, tt.label)
			}
		})
	}
}

func TestGetUserInfoTreatsTrialProbeFailureAsUnknown(t *testing.T) {
	client := &UpstreamClient{token: "token", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/me":
			return upstreamTestResponse(200, `{"email":"user@example.com"}`), nil
		case "/backend-api/conversation/init":
			return upstreamTestResponse(200, `{"limits_progress":[]}`), nil
		case "/backend-api/accounts/check/v4-2023-04-27":
			return upstreamTestResponse(503, "unavailable"), nil
		default:
			t.Fatalf("unexpected account info path %s", req.URL.Path)
			return nil, nil
		}
	})}
	info, err := client.GetUserInfo(context.Background())
	if err != nil || info.TrialState != "unknown" || info.TrialCheckedAt == "" {
		t.Fatalf("trial probe failure: state=%q checked=%q err=%v", info.TrialState, info.TrialCheckedAt, err)
	}
}

func TestGetUserInfoUsesTokenAccountForTrial(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-123"}}`))
	client := &UpstreamClient{token: "header." + payload + ".signature", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/me":
			return upstreamTestResponse(200, `{"email":"user@example.com"}`), nil
		case "/backend-api/conversation/init":
			return upstreamTestResponse(200, `{"limits_progress":[]}`), nil
		case "/backend-api/accounts/check/v4-2023-04-27":
			if req.Header.Get("ChatGPT-Account-Id") != "account-123" {
				t.Fatalf("account header = %q", req.Header.Get("ChatGPT-Account-Id"))
			}
			return upstreamTestResponse(200, `{"accounts":{"default":{"account":{"plan_type":"free"}},"account-123":{"account":{"plan_type":"free"},"eligible_promo_campaigns":{"plus":{"id":"plus-1-month-free"}}}}}}`), nil
		default:
			t.Fatalf("unexpected account info path %s", req.URL.Path)
			return nil, nil
		}
	})}
	info, err := client.GetUserInfo(context.Background())
	if err != nil || info.TrialState != "eligible" || info.TrialPromoLabel != "一个月免费" {
		t.Fatalf("token account trial: state=%q label=%q err=%v", info.TrialState, info.TrialPromoLabel, err)
	}
}

func TestChineseImageQuotaFailureSetsRestoreTime(t *testing.T) {
	err := errors.New("你已达到 Free 套餐的图像生成请求上限。上限将在 12小时 后重置")
	if !isRateLimitErrorText(err) || rateLimitRestoreDelay(err) != 12*time.Hour {
		t.Fatalf("quota error classification=%t delay=%s", isRateLimitErrorText(err), rateLimitRestoreDelay(err))
	}
}
