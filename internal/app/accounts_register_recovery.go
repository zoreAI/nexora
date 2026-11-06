package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tokenless registrations stay outside the serving pool until OAuth succeeds.
type pendingRegisterAccount struct {
	Email          string `json:"email"`
	Status         string `json:"status"`
	State          string `json:"state"`
	UpdatedAt      string `json:"updated_at"`
	PasswordSaved  bool   `json:"password_saved"`
	PasswordStatus string `json:"password_status"`
	RecoveryStatus string `json:"recovery_status"`
	RecoveryHint   string `json:"recovery_hint"`
}

// Older records only have an error string. Use the final attempt's evidence,
// excluding the earlier Platform error included for diagnostics. A Platform
// rejection alone does not describe the result of Web continuation.
func pendingRegisterRecoveryInfo(state string) (string, string) {
	current, _, _ := strings.Cut(state, "；此前 Platform 接续失败:")
	if strings.Contains(current, "/verify-your-identity") {
		return "identity_verification", "上游要求身份验证，请在官方页面手动完成，再重新检查或导入账号。"
	}
	if strings.Contains(current, "ChatGPT Web") && strings.Contains(current, "registration_disallowed") {
		return "registration_disallowed", "上游拒绝创建账号，请先在官方页面确认原因；处理后可重新检查。"
	}
	return "pending", "尚未取得登录令牌，完成注册、密码和 2FA 后才可用。"
}

func (s *Server) pendingRegisterAccounts() []pendingRegisterAccount {
	return s.pendingRegisterAccountsFrom(s.store.LoadAccounts())
}

func (s *Server) pendingRegisterAccountsFrom(accounts []Account) []pendingRegisterAccount {
	out := []pendingRegisterAccount{}
	if s.register == nil {
		return out
	}
	existing := map[string]bool{}
	for _, a := range accounts {
		existing[strings.ToLower(strings.TrimSpace(ptrString(a.Email)))] = true
	}
	cfg := s.register.snapshot()
	positions := map[string]int{}
	add := func(email, state, updated string, password bool) {
		email = strings.TrimSpace(email)
		key := strings.ToLower(email)
		if email == "" || existing[key] {
			return
		}
		if idx, ok := positions[key]; ok {
			if state != "" {
				out[idx].State = state
				out[idx].UpdatedAt = updated
			}
			out[idx].PasswordSaved = out[idx].PasswordSaved || password
			return
		}
		positions[key] = len(out)
		out = append(out, pendingRegisterAccount{Email: email, Status: "异常", State: state, UpdatedAt: updated, PasswordSaved: password, PasswordStatus: "unverified"})
	}
	for _, p := range cfg.PendingPasswords {
		add(p.Email, p.State, p.UpdatedAt, strings.TrimSpace(p.Password) != "")
		if i, ok := positions[strings.ToLower(strings.TrimSpace(p.Email))]; ok && p.PasswordStatus == "confirmed" {
			out[i].PasswordStatus = "confirmed"
		}
	}
	for _, p := range cfg.RecoveryEmails {
		add(p.Email, p.Step, p.UpdatedAt, false)
	}
	for i := range out {
		out[i].RecoveryStatus, out[i].RecoveryHint = pendingRegisterRecoveryInfo(out[i].State)
	}
	return out
}

func (s *Server) selectPendingRegisterEmails(emails []string, retryBlocked ...bool) []string {
	includeBlocked := len(retryBlocked) > 0 && retryBlocked[0]
	want := map[string]bool{}
	for _, email := range emails {
		want[strings.ToLower(strings.TrimSpace(email))] = true
	}
	out := []string{}
	for _, pending := range s.pendingRegisterAccounts() {
		if want[strings.ToLower(pending.Email)] && (pending.RecoveryStatus == "pending" || includeBlocked) {
			out = append(out, pending.Email)
		}
	}
	return out
}

func (s *registerService) savePendingMailbox(email string, mailbox *registerMailbox) error {
	raw, err := json.Marshal(mailbox)
	if err != nil {
		return err
	}
	var copy registerMailbox
	if err := json.Unmarshal(raw, &copy); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.PendingPasswords {
		if strings.EqualFold(s.cfg.PendingPasswords[i].Email, email) {
			s.cfg.PendingPasswords[i].Mailbox = &copy
			return s.saveLocked()
		}
	}
	return errors.New("待接续密码记录不存在")
}

func isRegisterProfileURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Host == "" || u.Host == "auth.openai.com") && strings.TrimRight(u.Path, "/") == "/about-you"
}

func (s *Server) hasRegisterRecovery(account Account) bool {
	email := strings.TrimSpace(ptrString(account.Email))
	return email != "" && s.register != nil && (s.register.hasRecoveryEmail(email) || s.register.pendingPasswordForEmail(email) != "")
}

func (s *Server) incompleteRegisterRecovery(account Account) bool {
	return s.hasRegisterRecovery(account) && !registerCredentialsComplete(account)
}

func registerCredentialsComplete(account Account) bool {
	return strings.TrimSpace(ptrString(account.Password)) != "" && account.PasswordStatus == "confirmed" && account.MFAStatus == "enabled" && strings.TrimSpace(ptrString(account.MFASecret)) != ""
}

// Snapshot before entering Store.UpdateAccounts: register saves acquire the
// service mutex before the store mutex, so never reverse that order in a callback.
func (s *Server) registerRecoveryEmailSet() map[string]bool {
	emails := map[string]bool{}
	if s.register == nil {
		return emails
	}
	cfg := s.register.snapshot()
	for _, p := range cfg.PendingPasswords {
		emails[strings.ToLower(strings.TrimSpace(p.Email))] = true
	}
	for _, p := range cfg.RecoveryEmails {
		emails[strings.ToLower(strings.TrimSpace(p.Email))] = true
	}
	delete(emails, "")
	return emails
}

func markIncompleteRegisterAccounts(accounts []Account, recovery map[string]bool) []Account {
	for i := range accounts {
		if accounts[i].Status == "正常" && recovery[strings.ToLower(strings.TrimSpace(ptrString(accounts[i].Email)))] && !registerCredentialsComplete(accounts[i]) {
			accounts[i].Status = "异常"
		}
	}
	return accounts
}

func (s *Server) reconcileRegisterRecoveryAccounts() error {
	recovery := s.registerRecoveryEmailSet()
	if len(recovery) == 0 {
		return nil
	}
	return s.store.UpdateAccounts(func(accounts []Account) []Account {
		return markIncompleteRegisterAccounts(accounts, recovery)
	})
}

func (s *Server) repairPendingRegistration(ctx context.Context, email string, taskIndexes ...int) error {
	if s.register == nil {
		return errors.New("注册服务不可用")
	}
	svc := s.register
	release, err := s.accountRecoveryLocks.acquire(ctx, accountOperationKey(email, ""))
	if err != nil {
		return err
	}
	defer release()
	cfg := svc.snapshot()
	if cfg.Enabled && cfg.Stats.JobKind != "repair_abnormal" {
		return errors.New("请先停止注册任务，再修复待接续账号")
	}
	for _, a := range s.store.LoadAccounts() {
		if strings.EqualFold(strings.TrimSpace(ptrString(a.Email)), email) {
			if a.Status == "正常" && a.MFAStatus == "enabled" && a.PasswordStatus == "confirmed" && ptrString(a.Password) != "" {
				return nil
			}
			if a.Status == "异常" || (a.Status == "正常" && s.incompleteRegisterRecovery(a)) {
				return s.repairAbnormalAccountLocked(ctx, a)
			}
			return errors.New("该邮箱已进入号池且状态已变化，请刷新列表")
		}
	}
	var pending registerPendingPassword
	for _, item := range cfg.PendingPasswords {
		if strings.EqualFold(item.Email, email) {
			pending = item
			break
		}
	}
	if strings.TrimSpace(pending.Password) == "" {
		pending.Password = randomPassword()
		if err := svc.savePendingPassword(email, pending.Password, "修复拟用密码，尚未确认"); err != nil {
			return err
		}
	}
	mailbox := registerMailbox{Address: email}
	if pending.Mailbox != nil {
		raw, err := json.Marshal(pending.Mailbox)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &mailbox); err != nil {
			return err
		}
	}
	mailbox.Address = email
	if mailbox.Provider == "" && strings.HasSuffix(strings.ToLower(email), "@icloud.com") {
		mailbox.Provider = "icloud_api"
	}
	// Recreate the original provider only for reading this mailbox, never claim
	// another alias or release the interrupted registration's mailbox.
	entries := []map[string]any{}
	enabledIndex := 0
	for _, entry := range cfg.Mail.Providers {
		if entry == nil || !boolAny(entry["enable"], true) {
			continue
		}
		enabledIndex++
		kind := strings.ToLower(strings.TrimSpace(strAny(entry["type"], "")))
		ref := firstNonEmpty(strAny(entry["provider_ref"], ""), fmt.Sprintf("%s#%d", kind, enabledIndex))
		if kind != mailbox.Provider || !boolAny(entry["enable"], true) {
			continue
		}
		if mailbox.ProviderRef != "" && ref != mailbox.ProviderRef {
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) != 1 {
		return errors.New("无法唯一确定原邮箱 provider，请检查配置后重试")
	}
	route, err := svc.accountRepairProxyRoute(ctx)
	if err != nil {
		return err
	}
	mailConfig := cfg.Mail
	mailConfig.Providers = entries
	provider, err := svc.createProviderWithProxy(mailConfig, route.URL)
	if err != nil {
		return err
	}
	defer provider.Close()
	session, ok := provider.Session().(*registerSession)
	if !ok {
		return errors.New("邮箱会话不支持注册接续")
	}
	if svc.recoverySessionFactory != nil {
		session, err = svc.recoverySessionFactory(route.URL)
		if err != nil {
			return err
		}
		defer session.close()
		provider = &recoverySessionProvider{registerProvider: provider, session: session}
	}
	session.proxyRoute = route
	releaseMFA, err := s.accountMFALocks.acquire(ctx, accountOperationKey(email, ""))
	if err != nil {
		return err
	}
	defer releaseMFA()
	index := 1
	if len(taskIndexes) > 0 {
		index = taskIndexes[0]
	}
	err = svc.resumePendingRegisterAccount(ctx, provider, &mailbox, pending.Password, index)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		message := err.Error()
		for _, secret := range []string{pending.Password, mailbox.Password, mailbox.Token, mailbox.RefreshToken, route.URL} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		_ = svc.recordRecoveryEmail(email, truncateText(message, 500))
		return errors.New(message)
	}
	return nil
}

type recoverySessionProvider struct {
	registerProvider
	session *registerSession
}

func (p *recoverySessionProvider) Session() registerProviderSession { return p.session }

// Both repair entry points must complete MFA before reporting a partial
// registration as repaired.
func (s *Server) repairAbnormalAccount(ctx context.Context, account Account) error {
	release, err := s.accountRecoveryLocks.acquire(ctx, accountOperationKey(ptrString(account.Email), account.AccessToken))
	if err != nil {
		return err
	}
	defer release()
	return s.repairAbnormalAccountLocked(ctx, account)
}

func (s *Server) repairAbnormalAccountLocked(ctx context.Context, account Account) error {
	if strings.TrimSpace(account.AccessToken) == "" {
		return errors.New("账号缺少登录令牌，请从注册待修复区域接续")
	}
	// A second repair request can have waited while the first rotated the token.
	found := false
	for _, current := range s.store.LoadAccounts() {
		if accountOperationKey(ptrString(current.Email), current.AccessToken) == accountOperationKey(ptrString(account.Email), account.AccessToken) {
			incomplete := s.incompleteRegisterRecovery(current)
			if current.Status == "正常" && current.MFAStatus != "pending" && current.MFAStatus != "failed" && !incomplete {
				return nil
			}
			if current.Status != "异常" && !(current.Status == "正常" && incomplete) {
				return errors.New("账号状态已变化，请刷新列表")
			}
			account = current
			found = true
			break
		}
	}
	if !found {
		return errors.New("账号已删除或令牌已变化")
	}
	if s.incompleteRegisterRecovery(account) {
		// Legacy imports may have a token but no completion flags or password.
		// Persist a candidate before any upstream work and keep this account out
		// of the serving pool until both password and MFA have been completed.
		candidate := firstNonEmpty(ptrString(account.Password), s.register.pendingPasswordForEmail(ptrString(account.Email)))
		if candidate == "" {
			candidate = randomPassword()
		}
		updated := false
		if err := s.store.UpdateAccounts(func(items []Account) []Account {
			for i := range items {
				if items[i].AccessToken != account.AccessToken || ptrString(items[i].Password) != ptrString(account.Password) {
					continue
				}
				if ptrString(items[i].Password) == "" {
					items[i].Password = &candidate
					items[i].PasswordStatus = "unverified"
				}
				items[i].Status = "异常"
				if items[i].MFAStatus != "enabled" {
					items[i].MFAStatus = "pending"
				}
				account = items[i]
				updated = true
				break
			}
			return items
		}); err != nil {
			return err
		}
		if !updated {
			return errors.New("账号、密码或令牌已变化，请刷新后重新修复")
		}
	}
	needsMFA := account.MFAStatus == "pending" || account.MFAStatus == "failed"
	if s.incompleteRegisterRecovery(account) && account.SourceType != "codex" {
		valid, err := s.validateRecoverySession(ctx, account)
		if err != nil {
			if isAccountDeactivatedError(err) {
				_ = s.recordReLoginFailure(account.AccessToken, err)
			}
			return err
		}
		if valid {
			reportAccountRepairProgress(ctx, "现有登录会话有效，直接补全密码和 2FA")
			return s.completeRecoveredAccount(ctx, account)
		}
	}
	repaired, skipped, errs, skippedItems := s.reLoginAccounts(ctx, []string{account.AccessToken})
	var browserRecovered *Account
	if repaired == 0 && !(needsMFA && skipped > 0 && len(errs) == 0) {
		// The batch API serializes errors into strings; retain the cancellation
		// cause for the repair coordinator instead of treating a stop as failure.
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		if len(errs) > 0 {
			message := strAny(errs[0]["error"], "重新授权失败")
			if s.canUseBrowserRepairFallback(account, message) {
				recovered, browserErr := s.reLoginAccountInBrowser(ctx, account)
				if browserErr == nil {
					browserRecovered = &recovered
				} else {
					if isAccountDeactivatedError(browserErr) {
						_ = s.recordReLoginFailure(account.AccessToken, browserErr)
					}
					return fmt.Errorf("浏览器回退失败: %w；直接授权失败: %s", browserErr, truncateText(message, 200))
				}
			} else {
				return errors.New(message)
			}
		}
		if browserRecovered == nil && skipped > 0 && len(skippedItems) > 0 {
			return errors.New(strAny(skippedItems[0]["reason"], "没有可用的重新授权方式"))
		}
		if browserRecovered == nil {
			return errors.New("账号已删除或状态已变化")
		}
	}
	var current Account
	if browserRecovered != nil {
		current = *browserRecovered
	} else {
		for _, item := range s.store.LoadAccounts() {
			if item.AccessToken == account.AccessToken || (strings.EqualFold(ptrString(item.Email), ptrString(account.Email)) && ptrString(account.Email) != "" && item.SourceType == account.SourceType) {
				current = item
				break
			}
		}
	}
	if current.AccessToken == "" {
		return errors.New("重新授权后找不到账号")
	}
	return s.completeRecoveredAccount(ctx, current)
}

// Validate identity before using an existing session to finish a partial
// registration. Only expired/revoked tokens fall back to reauthorization;
// transport failures and identity mismatches must not start another OTP flow.
func (s *Server) validateRecoverySession(ctx context.Context, account Account) (bool, error) {
	reportAccountRepairProgress(ctx, "校验现有登录会话")
	route, err := s.register.accountRepairProxyRoute(ctx)
	if err != nil {
		return false, err
	}
	session, err := s.newAccountLoginSession(route.URL, 25)
	if err != nil {
		return false, err
	}
	defer session.close()
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	body, err := session.requestText(checkCtx, http.MethodGet, "https://chatgpt.com/backend-api/me", map[string]string{
		"Authorization": "Bearer " + account.AccessToken,
		"Accept":        "application/json", "Referer": "https://chatgpt.com/",
	}, nil, http.StatusOK)
	if err != nil {
		if !isAccountDeactivatedError(err) && isInvalidTokenErrorText(err) {
			return false, nil
		}
		return false, fmt.Errorf("校验现有登录会话失败: %w", err)
	}
	var identity struct {
		Email string `json:"email"`
	}
	if json.Unmarshal([]byte(body), &identity) != nil || strings.TrimSpace(identity.Email) == "" || !strings.EqualFold(strings.TrimSpace(identity.Email), strings.TrimSpace(ptrString(account.Email))) {
		return false, errors.New("现有登录会话邮箱与账号不一致或响应缺少邮箱，保留待修复状态")
	}
	return true, nil
}

// Password and MFA completion share one persistence path for both an existing
// valid session and a newly recovered session.
func (s *Server) completeRecoveredAccount(ctx context.Context, current Account) error {
	if current.MFAStatus == "enabled" && strings.TrimSpace(ptrString(current.MFASecret)) == "" && s.hasRegisterRecovery(current) {
		return errors.New("账号标记为已启用 2FA，但未保存密钥，请手动补录原 2FA 密钥后再修复")
	}
	needsMFA := current.MFAStatus == "pending" || current.MFAStatus == "failed" || (s.incompleteRegisterRecovery(current) && current.MFAStatus != "enabled")
	if ptrString(current.Password) != "" && current.PasswordStatus != "confirmed" {
		if s.register == nil {
			return errors.New("注册服务不可用，登录密码尚未确认")
		}
		if err := s.store.UpdateAccounts(func(items []Account) []Account {
			for i := range items {
				if items[i].AccessToken == current.AccessToken {
					items[i].Status = "异常"
				}
			}
			return items
		}); err != nil {
			return err
		}
		route, err := s.register.accountRepairProxyRoute(ctx)
		if err != nil {
			return err
		}
		session, err := s.newAccountLoginSession(route.URL, 60)
		if err != nil {
			return err
		}
		defer session.close()
		session.proxyRoute = route
		current, err = s.register.confirmRecoveredPassword(ctx, session, current, func(ctx context.Context, notBefore time.Time) (string, error) {
			return s.waitAccountEmailCode(ctx, session, current, ptrString(current.Email), ptrString(current.EmailPassword), notBefore)
		})
		if err != nil {
			return err
		}
	}
	if !needsMFA {
		current.Status = "正常"
		if err := s.persistReLoginAccount(current.AccessToken, current); err != nil {
			return err
		}
		s.clearCompletedRegisterRecovery(s.store.LoadAccounts())
		return nil
	}
	releaseMFA, err := s.accountMFALocks.acquire(ctx, accountOperationKey(ptrString(current.Email), current.AccessToken))
	if err != nil {
		return err
	}
	defer releaseMFA()
	for _, item := range s.store.LoadAccounts() {
		if item.AccessToken == current.AccessToken && item.MFAStatus == "enabled" {
			item.Status = "正常"
			return s.persistReLoginAccount(item.AccessToken, item)
		}
	}
	if err := s.store.UpdateAccounts(func(items []Account) []Account {
		for i := range items {
			if items[i].AccessToken == current.AccessToken {
				items[i].Status = "异常"
			}
		}
		return items
	}); err != nil {
		return err
	}
	if s.register == nil {
		return errors.New("注册服务不可用，2FA 未完成")
	}
	route, err := s.register.accountRepairProxyRoute(ctx)
	if err != nil {
		return err
	}
	factory := s.register.mfaSessionFactory
	if factory == nil {
		factory = func(proxy string) (*registerSession, error) {
			return newRegisterSession(proxy, 45, s.ensureCurlImpersonateBinary)
		}
	}
	session, err := factory(route.URL)
	if err != nil {
		return err
	}
	defer session.close()
	session.proxyRoute = route
	reportAccountRepairProgress(ctx, "激活并保存 2FA")
	if err := s.register.enableRegisterMFAWithFailover(ctx, session, current.AccessToken, uuid4()); err != nil {
		return fmt.Errorf("2FA 未完成: %w", err)
	}
	s.clearCompletedRegisterRecovery(s.store.LoadAccounts())
	return nil
}

func (s *Server) canUseBrowserRepairFallback(account Account, message string) bool {
	if s.register == nil || strings.TrimSpace(ptrString(account.Email)) == "" || strings.TrimSpace(ptrString(account.Password)) == "" {
		return false
	}
	if registerMailboxReceivedNoCode(errors.New(message)) {
		return true
	}
	text := strings.ToLower(message)
	// Browser fallback is reserved for a missing callback/session transition. A
	// bad password, deactivated account or provider outage must keep its original
	// error and status.
	return strings.Contains(text, "oauth callback code") || strings.Contains(text, "session is no longer valid") || strings.Contains(text, "invalid_state")
}

func (s *Server) reLoginAccountInBrowser(ctx context.Context, old Account) (Account, error) {
	email := strings.TrimSpace(ptrString(old.Email))
	password := strings.TrimSpace(ptrString(old.Password))
	if email == "" || password == "" {
		return Account{}, errors.New("账号缺少邮箱或登录密码")
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
	cfg := s.register.snapshot()
	session.loginMFASecret = ptrString(old.MFASecret)
	session.onPasswordAccepted = func() error { _, err := s.persistConfirmedPassword(old, old); return err }
	profile := registerProfileConfig{Name: cfg.ProfileName, Birthdate: cfg.ProfileBirthdate}
	var mailbox registerMailbox
	if s.canReadAccountEmailCode(old) {
		mailbox = registerMailbox{Address: email, Provider: "icloud_api"}
	}
	setup := s.register.signupBrowserFactory
	if setup == nil {
		setup = registerPendingInBrowser
	}
	tokens, err := setup(ctx, session, email, password, profile, func(codeCtx context.Context, cutoff time.Time) (string, error) {
		if !s.canReadAccountEmailCode(old) {
			return "", errors.New("账号没有可用的邮箱验证码读取方式")
		}
		mailbox.CodeNotBefore = cutoff
		return s.waitAccountEmailCode(codeCtx, session, old, email, ptrString(old.EmailPassword), cutoff)
	})
	if err != nil && isInvalidLoginPassword(err) {
		// The stored candidate may belong to a registration that stopped before
		// the password request was accepted. Reuse the same mailbox and start
		// the password-reset authorization instead of treating the candidate as
		// the account's current password.
		reportAccountRepairProgress(ctx, "保存密码未生效，改用邮箱验证码重置密码")
		resetMailbox := registerMailbox{Address: email, Provider: "icloud_api"}
		resetTokens, resetErr := setRegisterPasswordInBrowser(ctx, session, &resetMailbox, password, route.URL, false, func(codeCtx context.Context, cutoff time.Time) (string, error) {
			if !s.canReadAccountEmailCode(old) {
				return "", errors.New("账号没有可用的邮箱验证码读取方式")
			}
			resetMailbox.CodeNotBefore = cutoff
			return s.waitAccountEmailCode(codeCtx, session, old, email, ptrString(old.EmailPassword), cutoff)
		})
		if resetErr == nil {
			resetTokens["password_confirmed"] = true
			tokens, err = resetTokens, nil
		} else {
			err = fmt.Errorf("候选密码登录失败，邮箱重置密码也未完成: %w", resetErr)
		}
	}
	if err != nil {
		return Account{}, err
	}
	at := strings.TrimSpace(strAny(tokens["access_token"], ""))
	if at == "" {
		return Account{}, errors.New("浏览器登录未返回 access_token")
	}
	if got := strings.TrimSpace(strAny(tokens["email"], "")); got != "" && !strings.EqualFold(got, email) {
		return Account{}, errors.New("浏览器登录邮箱与原账号不一致")
	}
	next := old
	next.AccessToken = at
	next.Status = "异常"
	next.SourceType = firstNonEmpty(next.SourceType, "web")
	if rt := strings.TrimSpace(strAny(tokens["refresh_token"], "")); rt != "" {
		next.RefreshToken = &rt
	} else {
		next.RefreshToken = nil
	}
	if id := strings.TrimSpace(strAny(tokens["id_token"], "")); id != "" {
		next.IDToken = &id
	} else {
		next.IDToken = nil
	}
	if boolAny(tokens["password_confirmed"], false) {
		next.PasswordStatus = "confirmed"
	}
	if err := s.store.UpdateAccounts(func(items []Account) []Account {
		for i := range items {
			if items[i].AccessToken == old.AccessToken || (strings.EqualFold(ptrString(items[i].Email), email) && items[i].SourceType == old.SourceType) {
				items[i].AccessToken = next.AccessToken
				items[i].RefreshToken = next.RefreshToken
				items[i].IDToken = next.IDToken
				items[i].Status = next.Status
				items[i].SourceType = next.SourceType
				items[i].PasswordStatus = next.PasswordStatus
				next = items[i]
				break
			}
		}
		return items
	}); err != nil {
		return Account{}, err
	}
	return next, nil
}
