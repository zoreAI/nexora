package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

func TestAccountQuotaRefreshDue(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	old := now.Add(-24 * time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		a    Account
		due  bool
	}{
		{"normal zero", Account{Status: "正常", QuotaCheckedAt: fresh}, true},
		{"limited", Account{Status: "限流", RateLimitResetAt: &future, QuotaCheckedAt: fresh}, true},
		{"reset reached with quota remaining", Account{Status: "正常", Quota: 2, RateLimitResetAt: &past, QuotaCheckedAt: fresh}, true},
		{"restore reached", Account{Status: "正常", Quota: 2, RestoreAt: &past, QuotaCheckedAt: fresh}, true},
		{"daily fallback", Account{Status: "正常", Quota: 2, QuotaCheckedAt: old}, true},
		{"legacy baseline", Account{Status: "正常", Quota: 2}, true},
		{"fresh healthy", Account{Status: "正常", Quota: 2, RateLimitResetAt: &future, QuotaCheckedAt: fresh}, false},
		{"fresh unknown", Account{Status: "正常", ImageQuotaUnknown: true, QuotaCheckedAt: fresh}, false},
		{"invalid reset", Account{Status: "正常", Quota: 2, RestoreAt: quotaTestString("invalid"), QuotaCheckedAt: fresh}, false},
		{"abnormal", Account{Status: "异常"}, false},
		{"banned", Account{Status: "封号"}, false},
		{"disabled", Account{Status: "禁用"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.a.AccessToken = "token"
			if got := accountQuotaRefreshDue(tc.a, now); got != tc.due {
				t.Fatalf("due = %v, want %v", got, tc.due)
			}
			tc.a.AccessToken = ""
			if accountQuotaRefreshDue(tc.a, now) {
				t.Fatal("account without credentials selected")
			}
		})
	}
}

func TestQuotaWatcherRestoresExhaustedAccountsOnStartup(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	old := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	fresh := nowISO()
	accounts := []Account{
		{AccessToken: "local-zero", Status: "正常", QuotaCheckedAt: fresh},
		{AccessToken: "upstream-zero", Status: "限流", RateLimitResetAt: &old},
		{AccessToken: "fresh", Status: "正常", Quota: 2, QuotaCheckedAt: fresh},
		{AccessToken: "disabled", Status: "禁用"},
		{AccessToken: "failed", Status: "正常", QuotaCheckedAt: fresh},
	}
	if err := store.SaveAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ImageAccountConcurrency: 1}
	s := &Server{store: store, accountPool: newAccountPool(&cfg)}
	s.setAccountQuotaRefreshInterval(60)
	if token, err := s.accountPool.pickTokenExcluding(accounts, false, "", map[string]bool{"fresh": true}); err == nil {
		t.Fatalf("exhausted accounts unexpectedly selectable before refresh: %s", token)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	seen := map[string]bool{}
	done := make(chan struct{})
	go func() {
		s.runAccountQuotaWatcher(ctx, accounts, func(ctx context.Context, a Account) error {
			mu.Lock()
			seen[a.AccessToken] = true
			if len(seen) == 3 {
				defer cancel()
			}
			mu.Unlock()
			if a.AccessToken == "failed" {
				return errors.New("temporary network failure")
			}
			client := &UpstreamClient{token: a.AccessToken, client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/backend-api/me":
					return upstreamTestResponse(200, `{}`), nil
				case "/backend-api/conversation/init":
					return upstreamTestResponse(200, `{"limits_progress":[{"feature_name":"image_gen","remaining":3}]}`), nil
				case "/backend-api/accounts/check/v4-2023-04-27":
					return upstreamTestResponse(200, `{"accounts":{}}`), nil
				default:
					return nil, errors.New("unexpected upstream path")
				}
			})}
			// Keep the test's cancellation separate from the simulated upstream response.
			info, err := client.GetUserInfo(context.Background())
			if err != nil {
				return err
			}
			return s.applyAccountInfoRefresh(a, info)
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("startup waited for the hourly timer instead of refreshing immediately")
	}
	if len(seen) != 3 || !seen["local-zero"] || !seen["upstream-zero"] || !seen["failed"] {
		t.Fatalf("selected accounts = %v", seen)
	}
	stored := store.LoadAccounts()
	for _, a := range stored[:2] {
		if a.Status != "正常" || a.Quota != 3 || a.QuotaCheckedAt == "" || accountQuotaRefreshDue(a, time.Now()) {
			t.Fatalf("recovered account = %+v", a)
		}
	}
	if stored[4].Quota != 0 || stored[4].Status != "正常" {
		t.Fatalf("network failure changed quota/status: %+v", stored[4])
	}
	if token, err := s.accountPool.pickTokenExcluding(stored, false, "", map[string]bool{"fresh": true}); err != nil || token != "local-zero" {
		t.Fatalf("recovered account not selectable: %q, %v", token, err)
	}
}

func TestQuotaRefreshRejectsConcurrentStateChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"quota", func(a *Account) { a.Quota = 2 }},
		{"usage", func(a *Account) { a.Success++ }},
		{"disabled", func(a *Account) { a.Status = "禁用" }},
		{"newer check", func(a *Account) { a.QuotaCheckedAt = nowISO() }},
		{"token rotated", func(a *Account) { a.AccessToken = "new-token" }},
		{"refresh token rotated", func(a *Account) { a.RefreshToken = quotaTestString("new-rt") }},
		{"new rate limit", func(a *Account) { a.RateLimitedAt = quotaTestString(nowISO()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			defer store.Close()
			observed := Account{AccessToken: "token", Status: "正常", RefreshToken: quotaTestString("old-rt")}
			if err := store.SaveAccounts([]Account{observed}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateAccounts(func(items []Account) []Account {
				tc.change(&items[0])
				return items
			}); err != nil {
				t.Fatal(err)
			}
			s := &Server{store: store}
			if err := s.applyAccountInfoRefresh(observed, Account{Status: "正常", Quota: 9}); err == nil {
				t.Fatal("stale refresh succeeded")
			}
			if a := store.LoadAccounts()[0]; a.Quota == 9 {
				t.Fatal("stale quota overwrote current account")
			}
		})
	}
}

func quotaTestString(value string) *string { return &value }

func TestQuotaRefreshReplacesOldResetAndPersistsCheckTime(t *testing.T) {
	old := "2026-11-04T00:00:00Z"
	next := "2026-11-06T00:00:00Z"
	for _, status := range []string{"正常", "限流"} {
		a := Account{Status: status, RestoreAt: &old, RateLimitResetAt: &old}
		mergeRefreshedAccount(&a, Account{Status: "正常", Quota: 3, RestoreAt: &next, RateLimitResetAt: &next, QuotaCheckedAt: nowISO()})
		if a.Status != "正常" || ptrString(a.RestoreAt) != next || ptrString(a.RateLimitResetAt) != next || a.QuotaCheckedAt == "" {
			t.Fatalf("new quota window not retained: %+v", a)
		}
		mergeRefreshedAccount(&a, Account{Status: "正常", ImageQuotaUnknown: true, QuotaCheckedAt: nowISO()})
		if a.RestoreAt != nil || a.RateLimitResetAt != nil || !a.ImageQuotaUnknown {
			t.Fatalf("obsolete quota window not cleared: %+v", a)
		}
	}
}

func TestQuotaWatcherIntervalChangesWhileWaiting(t *testing.T) {
	s := &Server{quotaRefreshChanged: make(chan struct{}, 1)}
	s.setAccountQuotaRefreshInterval(0)
	if got := time.Duration(s.quotaRefreshInterval.Load()); got != 5*time.Minute {
		t.Fatalf("default interval = %s", got)
	}
	<-s.quotaRefreshChanged
	s.quotaRefreshInterval.Store(int64(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.waitForAccountQuotaRefresh(ctx)
		close(done)
	}()
	s.quotaRefreshInterval.Store(int64(5 * time.Millisecond))
	s.quotaRefreshChanged <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interval change did not reschedule the pending timer")
	}
}
