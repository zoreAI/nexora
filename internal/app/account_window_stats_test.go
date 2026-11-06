package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAccountWindowStatsFollowUpstreamWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour).Format(time.RFC3339)
	a := Account{Success: 50, Fail: 12, RestoreAt: &reset}
	refreshAccountWindowStats(&a, Account{RestoreAt: &reset}, now)
	if a.WindowSuccess != 0 || a.WindowFail != 0 || a.WindowStartedAt == "" {
		t.Fatal("lifetime counts were attributed to a new window")
	}
	a.WindowSuccess, a.WindowFail = 5, 4
	equivalent := now.Add(time.Hour).In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
	refreshAccountWindowStats(&a, Account{RestoreAt: &equivalent}, now.Add(time.Minute))
	if a.WindowSuccess != 5 || a.WindowFail != 4 {
		t.Fatal("same reset instant discarded window statistics")
	}
	// Refreshing quota, including an unknown response, is not a new window.
	refreshAccountWindowStats(&a, Account{ImageQuotaUnknown: true}, now.Add(2*time.Minute))
	if a.WindowSuccess != 5 || a.WindowFail != 4 {
		t.Fatal("unknown quota discarded window statistics")
	}
	// Calls can finish after the old reset but before quota refresh completes.
	advanceAccountWindowStats(&a, now.Add(time.Hour))
	a.WindowSuccess++
	advanceAccountWindowStats(&a, now.Add(time.Hour+time.Minute))
	a.WindowFail++
	nextReset := now.Add(25 * time.Hour).Format(time.RFC3339)
	refreshAccountWindowStats(&a, Account{RestoreAt: &nextReset}, now.Add(time.Hour+2*time.Minute))
	if a.WindowSuccess != 1 || a.WindowFail != 1 || a.WindowResetAt != nextReset || a.Success != 50 || a.Fail != 12 {
		t.Fatal("refresh lost calls recorded after the reset or modified lifetime counts")
	}
	// An upstream-confirmed replacement of the active window also rebases.
	newReset := now.Add(49 * time.Hour).Format(time.RFC3339)
	refreshAccountWindowStats(&a, Account{RestoreAt: &newReset}, now.Add(2*time.Hour))
	if a.WindowSuccess != 0 || a.WindowFail != 0 || a.WindowResetAt != newReset {
		t.Fatal("new upstream window retained old counts")
	}
}

func TestAccountWindowStatsPersistAndRollOverOnCalls(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	s := &Server{store: store}
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	a := Account{AccessToken: "token", Status: "正常", Quota: 5, Success: 50, Fail: 12, RestoreAt: &reset}
	if err := store.SaveAccounts([]Account{a}); err != nil {
		t.Fatal(err)
	}
	s.markAccountSuccess("token", false)
	s.markAccountSuccess("token", true)
	s.markAccountFailure("token", errors.New("upstream failed"), true, "gpt-image-2")
	s.markAccountFailure("token", context.Canceled, true, "gpt-image-2")
	current := store.LoadAccounts()[0]
	if current.Success != 52 || current.Fail != 13 || current.WindowSuccess != 2 || current.WindowFail != 1 || current.Quota != 4 {
		t.Fatal("window call counts did not persist independently from lifetime totals")
	}
	// A temporary local rate limit must not change the upstream stats boundary.
	s.markAccountFailure("token", errors.New("status=429 try again in 5 minutes"), true, "gpt-image-2")
	current = store.LoadAccounts()[0]
	if current.WindowResetAt != reset || current.WindowSuccess != 2 || current.WindowFail != 2 {
		t.Fatal("local cooldown started a different statistics window")
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := store.UpdateAccounts(func(accounts []Account) []Account {
		accounts[0].WindowResetAt = past
		return accounts
	}); err != nil {
		t.Fatal(err)
	}
	s.markAccountSuccess("token", true)
	current = store.LoadAccounts()[0]
	if current.WindowSuccess != 1 || current.WindowFail != 0 || current.Success != 53 || current.Fail != 14 {
		t.Fatal("new-window call did not reset the window counts")
	}
	applyAccountFailureState(&current, errors.New("status=401"), nowISO())
	if current.WindowSuccess != 1 || current.WindowFail != 1 || current.Fail != 15 {
		t.Fatal("account authorization failure did not use the same window")
	}
}

func TestAccountListHidesExpiredWindowStatsWithoutErasingTotals(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	s := &Server{store: store, cfg: Config{AuthKey: "admin"}}
	a := Account{AccessToken: "token", Status: "正常", Success: 50, Fail: 12, WindowSuccess: 5, WindowFail: 4,
		WindowStartedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339), WindowResetAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	if err := store.SaveAccounts([]Account{a}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	s.handleAccounts(rec, req)
	var result struct {
		Items []Account `json:"items"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Items) != 1 {
		t.Fatal("account list failed")
	}
	got := result.Items[0]
	if got.WindowSuccess != 0 || got.WindowFail != 0 || got.Success != 50 || got.Fail != 12 {
		t.Fatal("account list reported expired counts or erased lifetime totals")
	}
	if stored := store.LoadAccounts()[0]; stored.WindowSuccess != 5 || stored.WindowFail != 4 {
		t.Fatal("read-only listing modified persisted statistics")
	}
}
