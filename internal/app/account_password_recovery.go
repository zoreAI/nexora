package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func normalizePasswordStatus(status string) string {
	if status == "confirmed" {
		return "confirmed"
	}
	return "unverified"
}

func isInvalidLoginPassword(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, code := range []string{"invalid_password", "wrong_password", "incorrect_password", "invalid_credentials", "invalid_username_or_password", "wrong email or password", "incorrect email or password"} {
		if strings.Contains(message, code) {
			return true
		}
	}
	return false
}

func isPasswordAlreadyUsedError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "password_already_used") || strings.Contains(text, "password already used")
}

// A locally stored candidate is never evidence of a password accepted upstream.
// Keep the OAuth token before this step, but keep the account abnormal until
// password setup succeeds. The browser authorization has its own CSRF session.
func (s *registerService) confirmRecoveredPassword(ctx context.Context, session *registerSession, account Account, waitCode func(context.Context, time.Time) (string, error)) (Account, error) {
	if account.PasswordStatus == "confirmed" {
		return account, nil
	}
	if ptrString(account.Password) == "" || ptrString(account.Email) == "" {
		return account, errors.New("账号尚无已确认的登录密码，需要补设密码")
	}
	previousHook, previousSecret := session.onPasswordAccepted, session.loginMFASecret
	session.loginMFASecret = ptrString(account.MFASecret)
	session.onPasswordAccepted = func() error { _, err := s.server.persistConfirmedPassword(account, account); return err }
	defer func() { session.onPasswordAccepted, session.loginMFASecret = previousHook, previousSecret }()
	reportAccountRepairProgress(ctx, "检查账号是否需要添加密码")
	endpoint := "https://chatgpt.com/backend-api/accounts/add_password/eligibility"
	text, err := session.requestText(ctx, http.MethodGet, endpoint, map[string]string{
		"Authorization": "Bearer " + account.AccessToken,
		"Accept":        "application/json", "Referer": "https://chatgpt.com/",
	}, nil, http.StatusOK)
	if err != nil {
		return account, fmt.Errorf("检查添加密码资格失败: %w", err)
	}
	var eligibility struct {
		Eligible *bool `json:"eligible"`
	}
	if err := json.Unmarshal([]byte(text), &eligibility); err != nil || eligibility.Eligible == nil {
		return account, errors.New("添加密码资格响应无效，未确认密码")
	}
	if *eligibility.Eligible {
		reportAccountRepairProgress(ctx, "账号尚未设置密码，开始邮箱验证并添加密码")
	} else {
		reportAccountRepairProgress(ctx, "本地密码尚未确认，开始邮箱验证并重置密码")
	}
	setup := s.passwordBrowserFactory
	if setup == nil {
		setup = func(ctx context.Context, session *registerSession, account Account, add bool, wait func(context.Context, time.Time) (string, error)) (map[string]any, error) {
			mailbox := &registerMailbox{Address: ptrString(account.Email)}
			return setRegisterPasswordInBrowser(ctx, session, mailbox, ptrString(account.Password), session.proxyRoute.URL, add, wait)
		}
	}
	tokens, err := setup(ctx, session, account, *eligibility.Eligible, waitCode)
	if err != nil {
		return account, fmt.Errorf("邮箱登录成功，但密码尚未确认: %w", err)
	}
	newToken := strings.TrimSpace(strAny(tokens["access_token"], ""))
	if newToken == "" || !strings.EqualFold(strings.TrimSpace(strAny(tokens["email"], "")), strings.TrimSpace(ptrString(account.Email))) {
		return account, errors.New("密码设置后未取得同一账号的登录会话，保留待修复状态")
	}
	next := account
	next.AccessToken = newToken
	next.PasswordStatus = "confirmed"
	next.Status = "异常" // MFA/recovery coordinator decides when the repair is complete.
	// A ChatGPT browser token does not come with the platform token's RT.
	if newToken != account.AccessToken {
		next.RefreshToken = nil
		next.IDToken = nil
		next.ExpiresAt = nil
	}
	saved, err := s.server.persistConfirmedPassword(account, next)
	if err != nil {
		return account, fmt.Errorf("密码已设置但保存确认状态失败: %w", err)
	}
	return saved, nil
}

// Do not confirm a different candidate edited while the browser was running.
// Retain concurrent changes to unrelated account fields.
func (s *Server) persistConfirmedPassword(before, next Account) (Account, error) {
	var saved Account
	err := s.store.UpdateAccounts(func(items []Account) []Account {
		for i := range items {
			current := items[i]
			if current.AccessToken != before.AccessToken || ptrString(current.Password) != ptrString(before.Password) || !strings.EqualFold(ptrString(current.Email), ptrString(before.Email)) {
				continue
			}
			current.AccessToken = next.AccessToken
			current.PasswordStatus = "confirmed"
			current.Status = "异常"
			current.RefreshToken = next.RefreshToken
			current.IDToken = next.IDToken
			current.ExpiresAt = next.ExpiresAt
			saved = current
			items[i] = current
			break
		}
		return items
	})
	if err != nil {
		return Account{}, err
	}
	if saved.AccessToken == "" {
		return Account{}, errors.New("账号、密码或令牌已变化，请刷新后重新修复")
	}
	return saved, nil
}
