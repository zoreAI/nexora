package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleAccountMFALogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if !readBody(w, r, &body) {
		return
	}
	var account *Account
	for _, item := range s.store.LoadAccounts() {
		if item.AccessToken == strings.TrimSpace(body.AccessToken) {
			account = &item
			break
		}
	}
	if account == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if account.Status != "异常" || ptrString(account.Email) == "" || ptrString(account.Password) == "" || ptrString(account.MFASecret) == "" {
		writeErr(w, http.StatusBadRequest, "仅支持已保存邮箱、密码和 2FA 密钥的异常账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	next, err := s.reLoginAccountWithPasswordSecret(ctx, *account, ptrString(account.MFASecret))
	if err != nil {
		if isAccountDeactivatedError(err) {
			if saveErr := s.recordReLoginFailure(account.AccessToken, err); saveErr != nil {
				writeErr(w, http.StatusInternalServerError, "保存账号异常状态失败")
				return
			}
		}
		message := err.Error()
		for _, secret := range []string{account.AccessToken, ptrString(account.Password), ptrString(account.MFASecret), ptrString(account.RefreshToken), ptrString(account.IDToken)} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		writeErr(w, http.StatusBadGateway, truncateText(message, 500))
		return
	}
	if err := s.persistReLoginAccount(account.AccessToken, next); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) reLoginAccountWithPassword(ctx context.Context, old Account) (Account, error) {
	return s.reLoginAccountWithPasswordSecret(ctx, old, "")
}

func (s *Server) newAccountLoginSession(proxy string, timeout int) (*registerSession, error) {
	if s.accountLoginSessionFactory != nil {
		return s.accountLoginSessionFactory(proxy, timeout)
	}
	return newRegisterSession(proxy, timeout, s.ensureCurlImpersonateBinary)
}

func (s *Server) reLoginAccountWithPasswordSecret(ctx context.Context, old Account, mfaSecret string) (Account, error) {
	email := ptrString(old.Email)
	password := ptrString(old.Password)
	if email == "" || password == "" {
		return Account{}, errors.New("账号缺少邮箱或密码")
	}
	route, err := s.accountLoginProxyRoute(ctx, old)
	if err != nil {
		return Account{}, err
	}
	session, err := s.newAccountLoginSession(route.URL, 45)
	if err != nil {
		return Account{}, err
	}
	defer session.close()
	session.proxyRoute = route
	// A legacy registration record may have been marked confirmed before the
	// account/profile step actually completed. When a pending/recovery marker is
	// still present, treat the stored password as a candidate and keep the email
	// OTP fallback available after a failed password attempt.
	recoveryCandidate := false
	if s.register != nil {
		recoveryCandidate = s.register.hasRecoveryEmail(email) || s.register.pendingPasswordForEmail(email) != ""
	}
	session.allowPasswordlessFallback = (old.PasswordStatus != "confirmed" || recoveryCandidate) && s.canReadAccountEmailCode(old)
	var profile *registerProfileConfig
	if s.register != nil && (old.MFAStatus == "pending" || old.MFAStatus == "failed" || s.register.hasRecoveryEmail(email) || s.register.pendingPasswordForEmail(email) != "") {
		cfg := s.register.snapshot()
		profile = &registerProfileConfig{Name: cfg.ProfileName, Birthdate: cfg.ProfileBirthdate}
	}
	tokens, err := session.loginWithPasswordContinuation(ctx, email, password, mfaSecret, profile, func(ctx context.Context, codeNotBefore time.Time) (string, error) {
		if !s.canReadAccountEmailCode(old) {
			return "", errors.New("账号没有可用的邮箱验证码读取方式，请检查原邮箱 provider 配置")
		}
		return s.waitAccountEmailCode(ctx, session, old, email, ptrString(old.EmailPassword), codeNotBefore)
	})
	if err != nil {
		return Account{}, err
	}
	accessToken := strings.TrimSpace(strAny(tokens["access_token"], ""))
	if accessToken == "" {
		return Account{}, errors.New("重新授权未返回 access_token")
	}
	refreshToken := strings.TrimSpace(strAny(tokens["refresh_token"], ""))
	idToken := strings.TrimSpace(strAny(tokens["id_token"], ""))
	now := nowISO()
	next := old
	if session.passwordConfirmed {
		next.PasswordStatus = "confirmed"
	} else {
		// Email login supplies no new password evidence. Preserve a prior
		// successful confirmation, but never upgrade a legacy candidate.
		next.PasswordStatus = normalizePasswordStatus(old.PasswordStatus)
	}
	next.AccessToken = accessToken
	next.Status = "正常"
	if _, repairing := ctx.Value(accountRepairProxyKey{}).(accountRepairProxy); repairing && next.PasswordStatus != "confirmed" {
		next.Status = "异常"
	}
	next.SourceType = firstNonEmpty(strings.TrimSpace(next.SourceType), "password")
	next.RateLimitedAt = nil
	next.RateLimitResetAt = nil
	next.RestoreAt = nil
	next.LastUsedAt = &now
	if refreshToken != "" {
		next.RefreshToken = &refreshToken
	}
	if idToken != "" {
		next.IDToken = &idToken
	}
	if expiresIn := intAny(tokens["expires_in"], 0); expiresIn > 0 {
		next.ExpiresAt = time.Now().Unix() + int64(expiresIn)
	}
	if accountID := chatGPTAccountID(accessToken); accountID != "" {
		next.AccountID = &accountID
	}
	return next, nil
}

func (s *Server) reLoginAccountWithEmailCode(ctx context.Context, old Account) (Account, error) {
	email := ptrString(old.Email)
	emailPassword := ptrString(old.EmailPassword)
	if emailPassword == "" && old.Mailbox != nil {
		emailPassword = strings.TrimSpace(strAny(old.Mailbox["password"], ""))
	}
	if email == "" || !s.canReadAccountEmailCode(old) {
		return Account{}, errors.New("账号缺少邮箱或可用的邮箱验证码读取方式")
	}
	route, err := s.accountLoginProxyRoute(ctx, old)
	if err != nil {
		return Account{}, err
	}
	session, err := s.newAccountLoginSession(route.URL, 60)
	if err != nil {
		return Account{}, err
	}
	defer session.close()
	session.proxyRoute = route
	deviceID := uuid4()
	codeNotBefore := time.Now().UTC().Truncate(time.Second)
	reportAccountRepairProgress(ctx, "开始邮箱验证码授权")
	codeVerifier, err := session.platformAuthorizeWithHint(ctx, deviceID, email, "login")
	if err != nil {
		codeVerifier, err = session.platformAuthorizeWithHint(ctx, deviceID, email, "login_or_signup")
		if err != nil {
			return Account{}, err
		}
	}
	if strings.HasPrefix(session.lastAuthorizeLanding, "choice path=") {
		if err := session.authorizeContinue(ctx, deviceID, email); err != nil {
			return Account{}, err
		}
	} else if !strings.HasPrefix(session.lastAuthorizeLanding, "verification path=") {
		return Account{}, fmt.Errorf("授权未进入邮箱验证码步骤: %s", session.lastAuthorizeLanding)
	}
	reportAccountRepairProgress(ctx, "请求邮箱验证码")
	if err := session.sendOTP(ctx, deviceID); err != nil {
		return Account{}, err
	}
	reportAccountRepairProgress(ctx, "等待邮箱验证码")
	code, err := s.waitAccountEmailCode(ctx, session, old, email, emailPassword, codeNotBefore)
	if err != nil {
		return Account{}, err
	}
	reportAccountRepairProgress(ctx, "校验邮箱验证码")
	continueURL, err := session.validateOTPContinueURL(ctx, deviceID, code)
	if err != nil {
		return Account{}, err
	}
	oauthCode := parseContinueCode(continueURL)
	if oauthCode == "" {
		return Account{}, errors.New("邮箱验证码已校验，但未返回 oauth callback code")
	}
	reportAccountRepairProgress(ctx, "获取登录令牌")
	tokens, err := session.exchangeTokens(ctx, codeVerifier, oauthCode)
	if err != nil {
		return Account{}, err
	}
	accessToken := strings.TrimSpace(strAny(tokens["access_token"], ""))
	if accessToken == "" {
		return Account{}, errors.New("邮箱验证码登录未返回 access_token")
	}
	refreshToken := strings.TrimSpace(strAny(tokens["refresh_token"], ""))
	idToken := strings.TrimSpace(strAny(tokens["id_token"], ""))
	now := nowISO()
	next := old
	if ptrString(next.Password) != "" {
		next.PasswordStatus = normalizePasswordStatus(old.PasswordStatus)
	}
	next.AccessToken = accessToken
	next.Status = "正常"
	next.SourceType = firstNonEmpty(strings.TrimSpace(next.SourceType), "email_code")
	next.RateLimitedAt = nil
	next.RateLimitResetAt = nil
	next.RestoreAt = nil
	next.LastUsedAt = &now
	if refreshToken != "" {
		next.RefreshToken = &refreshToken
	}
	if idToken != "" {
		next.IDToken = &idToken
	}
	if expiresIn := intAny(tokens["expires_in"], 0); expiresIn > 0 {
		next.ExpiresAt = time.Now().Unix() + int64(expiresIn)
	}
	if accountID := chatGPTAccountID(accessToken); accountID != "" {
		next.AccountID = &accountID
	}
	return next, nil
}

func (s *Server) waitAccountEmailCode(ctx context.Context, session *registerSession, account Account, email, emailPassword string, codeNotBefore time.Time) (string, error) {
	if s.canReadICloudAccountEmailCode(account) {
		return s.waitICloudAccountEmailCode(ctx, session, email, codeNotBefore)
	}
	if account.Mailbox != nil {
		clientID := strings.TrimSpace(strAny(account.Mailbox["client_id"], ""))
		refreshToken := strings.TrimSpace(strAny(account.Mailbox["refresh_token"], ""))
		provider := strings.ToLower(strings.TrimSpace(strAny(account.Mailbox["provider"], "")))
		if clientID != "" && refreshToken != "" && (provider == "" || strings.Contains(provider, "outlook")) {
			p := &outlookTokenRegisterProvider{
				registerProviderBase: registerProviderBase{sess: session},
				entry:                map[string]any{"wait_timeout": 90, "wait_interval": 3},
				statePath:            s.store.path("outlook_token_used.json"),
				store:                s.store,
			}
			return p.WaitForCode(ctx, &registerMailbox{
				Provider:      "outlook_token",
				Address:       email,
				Token:         refreshToken,
				ClientID:      clientID,
				Password:      emailPassword,
				CodeNotBefore: codeNotBefore,
			})
		}
	}
	loginEmail := email
	imapHost := ""
	if account.Mailbox != nil {
		loginEmail = firstNonEmpty(
			strAny(account.Mailbox["login_email"], ""),
			strAny(account.Mailbox["mailbox_email"], ""),
			strAny(account.Mailbox["mailbox_login_email"], ""),
			email,
		)
		imapHost = strAny(account.Mailbox["imap_host"], "")
	}
	return waitForIMAPCodeWithLogin(ctx, email, loginEmail, emailPassword, imapHost, codeNotBefore, 90, 3)
}

func (s *Server) canReadICloudAccountEmailCode(account Account) bool {
	email := strings.ToLower(strings.TrimSpace(ptrString(account.Email)))
	if account.Mailbox != nil && strings.EqualFold(strings.TrimSpace(strAny(account.Mailbox["provider"], "")), "icloud_api") {
		return true
	}
	if !strings.HasSuffix(email, "@icloud.com") || s.register == nil {
		return false
	}
	for _, entry := range s.register.snapshot().Mail.Providers {
		if strings.EqualFold(strings.TrimSpace(strAny(entry["type"], "")), "icloud_api") && boolAny(entry["enable"], true) {
			return true
		}
	}
	return false
}

func (s *Server) canReadAccountEmailCode(account Account) bool {
	if ptrString(account.Email) == "" {
		return false
	}
	if s.canReadICloudAccountEmailCode(account) || ptrString(account.EmailPassword) != "" {
		return true
	}
	if account.Mailbox == nil {
		return false
	}
	return strings.TrimSpace(strAny(account.Mailbox["password"], "")) != "" ||
		(strings.TrimSpace(strAny(account.Mailbox["client_id"], "")) != "" && strings.TrimSpace(strAny(account.Mailbox["refresh_token"], "")) != "")
}

func (s *Server) waitICloudAccountEmailCode(ctx context.Context, session *registerSession, email string, codeNotBefore time.Time) (string, error) {
	if s.register == nil {
		return "", errors.New("iCloud Privacy Mail 未配置")
	}
	mail := s.register.snapshot().Mail
	for _, configured := range mail.Providers {
		if !strings.EqualFold(strings.TrimSpace(strAny(configured["type"], "")), "icloud_api") || !boolAny(configured["enable"], true) {
			continue
		}
		entry := make(map[string]any, len(configured)+2)
		for key, value := range configured {
			entry[key] = value
		}
		if entry["wait_timeout"] == nil {
			entry["wait_timeout"] = mail.WaitTimeout
		}
		if entry["wait_interval"] == nil {
			entry["wait_interval"] = mail.WaitInterval
		}
		provider := &iCloudAPIRegisterProvider{registerProviderBase: registerProviderBase{sess: session}, entry: entry}
		if !mail.APIUseRegisterProxy {
			var err error
			provider.mailSess, err = newRegisterSession("", mail.RequestTimeout, nil)
			if err != nil {
				return "", err
			}
			defer provider.mailSess.close()
		}
		return provider.WaitForCode(ctx, &registerMailbox{Provider: "icloud_api", Address: email, CodeNotBefore: codeNotBefore})
	}
	return "", errors.New("iCloud Privacy Mail provider 未启用")
}

func (s *Server) reLoginAccounts(ctx context.Context, tokens []string) (int, int, []map[string]any, []map[string]any) {
	want := map[string]bool{}
	for _, token := range tokens {
		if strings.TrimSpace(token) != "" {
			want[strings.TrimSpace(token)] = true
		}
	}
	accounts := s.store.LoadAccounts()
	relogined := 0
	skipped := 0
	skippedItems := []map[string]any{}
	errs := []map[string]any{}
	for i := range accounts {
		if len(want) > 0 && !want[accounts[i].AccessToken] {
			continue
		}
		if ptrString(accounts[i].Password) == "" {
			var refreshErr error
			if ptrString(accounts[i].RefreshToken) != "" {
				itemCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
				next, err := s.refreshOAuthAccount(itemCtx, accounts[i].AccessToken)
				cancel()
				if err != nil {
					refreshErr = err
				} else {
					next.Status = "正常"
					if err := s.persistReLoginAccount(next.AccessToken, next); err != nil {
						errs = append(errs, map[string]any{"token": accounts[i].AccessToken, "email": ptrString(accounts[i].Email), "error": err.Error()})
						continue
					}
					accounts[i] = next
					relogined++
					continue
				}
			}
			if s.canReadAccountEmailCode(accounts[i]) {
				itemCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
				next, err := s.reLoginAccountWithEmailCode(itemCtx, accounts[i])
				cancel()
				if err != nil {
					if refreshErr != nil {
						err = fmt.Errorf("%s；邮箱验证码登录失败: %w", refreshErr.Error(), err)
					}
					if saveErr := s.recordReLoginFailure(accounts[i].AccessToken, err); saveErr != nil {
						err = fmt.Errorf("%w；保存账号状态失败: %v", err, saveErr)
					}
					errs = append(errs, map[string]any{"token": accounts[i].AccessToken, "email": ptrString(accounts[i].Email), "error": err.Error()})
					continue
				}
				if err := s.persistReLoginAccount(accounts[i].AccessToken, next); err != nil {
					errs = append(errs, map[string]any{"token": accounts[i].AccessToken, "email": ptrString(accounts[i].Email), "error": err.Error()})
					continue
				}
				relogined++
				continue
			}
			if refreshErr != nil {
				if saveErr := s.recordReLoginFailure(accounts[i].AccessToken, refreshErr); saveErr != nil {
					refreshErr = fmt.Errorf("%w；保存账号状态失败: %v", refreshErr, saveErr)
				}
				errs = append(errs, map[string]any{"token": accounts[i].AccessToken, "email": ptrString(accounts[i].Email), "error": refreshErr.Error()})
				continue
			}
			skipped++
			skippedItems = append(skippedItems, map[string]any{
				"token":  accounts[i].AccessToken,
				"email":  ptrString(accounts[i].Email),
				"reason": "账号缺少登录密码、refresh_token 或可取码的邮箱",
			})
			continue
		}
		if ptrString(accounts[i].Email) == "" {
			skipped++
			skippedItems = append(skippedItems, map[string]any{
				"token":  accounts[i].AccessToken,
				"email":  ptrString(accounts[i].Email),
				"reason": "账号缺少邮箱，无法密码登录",
			})
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
		next, err := s.reLoginAccountWithPasswordSecret(itemCtx, accounts[i], ptrString(accounts[i].MFASecret))
		cancel()
		if err != nil {
			if saveErr := s.recordReLoginFailure(accounts[i].AccessToken, err); saveErr != nil {
				err = fmt.Errorf("%w；保存账号状态失败: %v", err, saveErr)
			}
			errs = append(errs, map[string]any{"token": accounts[i].AccessToken, "email": ptrString(accounts[i].Email), "error": err.Error()})
			continue
		}
		if err := s.persistReLoginAccount(accounts[i].AccessToken, next); err != nil {
			errs = append(errs, map[string]any{"token": accounts[i].AccessToken, "email": ptrString(accounts[i].Email), "error": err.Error()})
			continue
		}
		relogined++
	}
	return relogined, skipped, errs, skippedItems
}

func (s *Server) recordReLoginFailure(token string, failure error) error {
	if errors.Is(failure, context.Canceled) {
		return nil
	}
	return s.store.UpdateAccounts(func(accounts []Account) []Account {
		for i := range accounts {
			if accounts[i].AccessToken == token {
				applyAccountFailureState(&accounts[i], failure, nowISO())
				break
			}
		}
		return accounts
	})
}

func (s *Server) persistReLoginAccount(oldToken string, next Account) error {
	updated := false
	err := s.store.UpdateAccounts(func(accounts []Account) []Account {
		for i := range accounts {
			if accounts[i].AccessToken != oldToken {
				continue
			}
			current := accounts[i]
			if ptrString(current.Password) == ptrString(next.Password) && next.PasswordStatus != "" {
				current.PasswordStatus = normalizePasswordStatus(next.PasswordStatus)
			}
			current.AccessToken = next.AccessToken
			current.Status = next.Status
			if next.Status == "正常" && (current.MFAStatus == "pending" || current.MFAStatus == "failed") {
				current.Status = "异常"
			}
			current.SourceType = next.SourceType
			current.RateLimitedAt = next.RateLimitedAt
			current.RateLimitResetAt = next.RateLimitResetAt
			current.RestoreAt = next.RestoreAt
			current.LastUsedAt = next.LastUsedAt
			current.RefreshToken = next.RefreshToken
			current.IDToken = next.IDToken
			current.ExpiresAt = next.ExpiresAt
			current.AccountID = next.AccountID
			accounts[i] = current
			updated = true
			break
		}
		return accounts
	})
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("账号已被删除或令牌已变化，重新授权结果未保存")
	}
	return nil
}
