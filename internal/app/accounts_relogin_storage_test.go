package app

import (
	"context"
	"fmt"
	"testing"
)

func TestReLoginCancellationDoesNotIncrementAccountFailures(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "异常", Fail: 2}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	if err := s.recordReLoginFailure("token", fmt.Errorf("OTP interrupted: %w", context.Canceled)); err != nil {
		t.Fatal(err)
	}
	if account := store.LoadAccounts()[0]; account.Fail != 2 || account.LastUsedAt != nil {
		t.Fatal("manual stop changed account failure state")
	}
	if err := s.recordReLoginFailure("token", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if store.LoadAccounts()[0].Fail != 3 {
		t.Fatal("real timeout must still count as failure")
	}
}

func TestPersistReLoginAccountPreservesConcurrentAccountChanges(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	if err := store.SaveAccounts([]Account{{AccessToken: "old-token", Quota: 1, Status: "异常"}}); err != nil {
		t.Fatal(err)
	}
	next := store.LoadAccounts()[0]
	next.AccessToken = "new-token"
	next.Status = "正常"
	if err := store.UpdateAccounts(func(accounts []Account) []Account {
		accounts[0].Quota = 9
		return append(accounts, Account{AccessToken: "new-account"})
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	if err := s.persistReLoginAccount("old-token", next); err != nil {
		t.Fatal(err)
	}
	accounts := store.LoadAccounts()
	if len(accounts) != 2 || accounts[0].AccessToken != "new-token" || accounts[0].Status != "正常" || accounts[0].Quota != 9 || accounts[1].AccessToken != "new-account" {
		t.Fatalf("concurrent account changes were lost: %+v", accounts)
	}
	if err := s.persistReLoginAccount("old-token", next); err == nil {
		t.Fatal("stale re-login result was accepted")
	}
}
