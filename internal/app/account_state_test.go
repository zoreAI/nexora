package app

import (
	"errors"
	"testing"
)

func TestInvalidRefreshTokenState(t *testing.T) {
	account := Account{Status: "正常", Quota: 3}
	err := errors.New(`GET /backend-api/me failed: status=401 body={"error":{"code":"unauthorized_unknown"}}`)
	if !isInvalidTokenErrorText(err) {
		t.Fatal("401 account refresh must be classified as an invalid token")
	}
	applyAccountFailureState(&account, err, "2026-10-15T00:00:00Z")
	if account.Status != "异常" || account.Quota != 0 || account.Fail != 1 {
		t.Fatalf("invalid token state: %+v", account)
	}

	if isInvalidTokenErrorText(errors.New("proxyconnect tcp: connection refused")) {
		t.Fatal("proxy failure must not invalidate an account")
	}
}

func TestAccountDeactivatedIsBannedButOther403IsNot(t *testing.T) {
	account := Account{Status: "正常", Quota: 5}
	err := errors.New(`POST /api/accounts/mfa/verify failed: status=403 code=account_deactivated type=invalid_request_error`)
	if !isAccountDeactivatedError(err) {
		t.Fatal("account_deactivated must be recognized")
	}
	applyAccountFailureState(&account, err, "2026-10-28T00:00:00Z")
	if account.Status != "封号" || account.Quota != 0 {
		t.Fatalf("deactivated account state: %+v", account)
	}
	applyAccountFailureState(&account, errors.New("status=401"), "2026-10-28T00:01:00Z")
	if account.Status != "封号" {
		t.Fatalf("later auth error changed banned state: %+v", account)
	}
	if isInvalidTokenErrorText(errors.New(`POST /api/accounts/mfa/verify failed: status=403 code=invalid_state`)) {
		t.Fatal("ordinary 403 must not mark the account deactivated")
	}
}

func TestBannedAccountIsNotSelectedFromPool(t *testing.T) {
	cfg := Config{ImageAccountConcurrency: 1}
	accounts := []Account{
		{AccessToken: "banned", Status: "封号", Type: "plus", Quota: 10},
		{AccessToken: "healthy", Status: "正常", Type: "plus", Quota: 10},
	}
	if token, err := newAccountPool(&cfg).pickToken(accounts, false); err != nil || token != "healthy" {
		t.Fatalf("image pool selected %q, err=%v", token, err)
	}
	if token, err := newAccountPool(&cfg).pickTextToken(accounts, ""); err != nil || token != "healthy" {
		t.Fatalf("text pool selected %q, err=%v", token, err)
	}
}
