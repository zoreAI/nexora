package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		accounts := s.store.LoadAccounts()
		now := time.Now()
		for i := range accounts {
			advanceAccountWindowStats(&accounts[i], now)
		}
		writeJSON(w, 200, map[string]any{"items": accounts, "pending_registrations": s.pendingRegisterAccountsFrom(accounts)})
	case http.MethodPost:
		var body struct {
			Tokens         []string         `json:"tokens"`
			Accounts       []map[string]any `json:"accounts"`
			AccountRecords []map[string]any `json:"account_records"`
			SourceType     string           `json:"source_type"`
		}
		if !readBody(w, r, &body) {
			return
		}
		source := strings.ToLower(strings.TrimSpace(body.SourceType))
		if source == "" {
			source = "web"
		}
		if source != "web" && source != "codex" {
			writeErr(w, 400, "source_type must be web or codex")
			return
		}
		tokset := map[string]map[string]any{}
		importedTokens := []string{}
		for _, t := range body.Tokens {
			t = strings.TrimSpace(t)
			if t != "" {
				if _, ok := tokset[t]; !ok {
					importedTokens = append(importedTokens, t)
				}
				tokset[t] = map[string]any{"access_token": t}
			}
		}
		for _, rec := range append(body.Accounts, body.AccountRecords...) {
			t := strings.TrimSpace(strAny(rec["access_token"], strAny(rec["accessToken"], "")))
			if t != "" {
				if _, ok := tokset[t]; !ok {
					importedTokens = append(importedTokens, t)
				}
				tokset[t] = rec
			}
		}
		if len(tokset) == 0 {
			writeErr(w, 400, "tokens, accounts or account_records is required")
			return
		}
		for _, rec := range tokset {
			if raw, ok := rec["group_id"]; ok {
				groupID, valid := raw.(string)
				if !valid {
					writeErr(w, http.StatusBadRequest, "group_id must be a string")
					return
				}
				if err := s.store.ValidateAccountGroupID(groupID); err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			raw, hasProxy := rec["proxy"]
			legacyGroup, hasLegacyGroup := rec["proxy_group_id"]
			if !hasProxy && !hasLegacyGroup {
				continue
			}
			assignment, ok := raw.(string)
			if hasProxy && !ok {
				writeErr(w, http.StatusBadRequest, "proxy must be a string")
				return
			}
			if !hasProxy || strings.TrimSpace(assignment) == "" && hasLegacyGroup {
				groupID, ok := legacyGroup.(string)
				if !ok {
					writeErr(w, http.StatusBadRequest, "proxy_group_id must be a string")
					return
				}
				if strings.TrimSpace(groupID) != "" {
					assignment = "group:" + strings.TrimSpace(groupID)
				}
			}
			normalized, err := s.store.normalizeAccountProxyAssignment(assignment)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "账号代理配置无效: "+err.Error())
				return
			}
			rec["proxy"] = normalized
		}
		added, skipped, signupTokens := 0, 0, 0
		recovery := s.registerRecoveryEmailSet()
		newTokens := map[string]bool{}
		for token := range tokset {
			if isSignupAccessToken(token) {
				signupTokens++
			}
		}
		if err := s.store.UpdateAccounts(func(accounts []Account) []Account {
			existing := map[string]int{}
			for i, a := range accounts {
				existing[a.AccessToken] = i
			}
			for token, rec := range tokset {
				a := accountFromRecord(token, source, rec)
				if idx, ok := existing[token]; ok {
					cur := accounts[idx]
					mergeImportedAccount(&cur, a, rec, body.SourceType != "")
					accounts[idx] = cur
					skipped++
				} else {
					accounts = append(accounts, a)
					newTokens[token] = true
					added++
				}
			}
			return markIncompleteRegisterAccounts(accounts, recovery)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存账号失败: "+err.Error())
			return
		}
		refreshed, errs, finalTokens := s.refreshAccountInfosWithFinalTokens(r.Context(), importedTokens)
		failed := map[string]bool{}
		for _, item := range errs {
			failed[strAny(item["token"], "")] = true
		}
		merged := 0
		recovery = s.registerRecoveryEmailSet()
		if err := s.store.UpdateAccounts(func(accounts []Account) []Account {
			preserveImportedPasswordEvidence(accounts, tokset, finalTokens)
			accounts, merged = mergeImportedEmails(accounts, importedTokens, finalTokens, newTokens, failed)
			return markIncompleteRegisterAccounts(accounts, recovery)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "合并同邮箱账号失败: "+err.Error())
			return
		}
		added = max(0, added-merged)
		accounts := s.store.LoadAccounts()
		s.clearCompletedRegisterRecovery(accounts)
		s.logSvc.add("account", "新增账号", map[string]any{"added": added, "skipped": skipped, "merged": merged, "refreshed": refreshed})
		writeJSON(w, 200, map[string]any{"added": added, "skipped": skipped, "merged": merged, "refreshed": refreshed, "signup_tokens": signupTokens, "errors": errs, "items": accounts})
	case http.MethodDelete:
		var body struct {
			Tokens []string `json:"tokens"`
		}
		if !readBody(w, r, &body) {
			return
		}
		targets := map[string]bool{}
		for _, t := range body.Tokens {
			if strings.TrimSpace(t) != "" {
				targets[strings.TrimSpace(t)] = true
			}
		}
		removed := 0
		var out []Account
		if err := s.store.UpdateAccounts(func(accounts []Account) []Account {
			out = make([]Account, 0, len(accounts))
			for _, a := range accounts {
				if targets[a.AccessToken] {
					removed++
				} else {
					out = append(out, a)
				}
			}
			return out
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "删除账号失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"removed": removed, "items": out})
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func accountEmailKey(email *string) string {
	return strings.ToLower(strings.TrimSpace(ptrString(email)))
}

func mergeImportedEmails(accounts []Account, importedTokens []string, finalTokens map[string]string, newTokens, failed map[string]bool) ([]Account, int) {
	merged := 0
	for _, originalToken := range importedTokens {
		token := originalToken
		if finalTokens[originalToken] != "" {
			token = finalTokens[originalToken]
		}
		importedIndex := -1
		for i := range accounts {
			if accounts[i].AccessToken == token {
				importedIndex = i
				break
			}
		}
		if importedIndex < 0 {
			continue
		}
		email := accountEmailKey(accounts[importedIndex].Email)
		if email == "" {
			continue
		}
		for i := 0; i < len(accounts); i++ {
			if i == importedIndex || accountEmailKey(accounts[i].Email) != email {
				continue
			}
			if failed[originalToken] {
				if !newTokens[originalToken] {
					break
				}
				accounts = append(accounts[:importedIndex], accounts[importedIndex+1:]...)
				merged++
				break
			}
			previous := accounts[i]
			current := accounts[importedIndex]
			mergeAccount(&previous, current)
			previous.AccessToken = current.AccessToken
			previous.IsSignupToken = current.IsSignupToken
			previous.Status = current.Status
			previous.Type = current.Type
			previous.SourceType = current.SourceType
			previous.Quota = current.Quota
			previous.ImageQuotaUnknown = current.ImageQuotaUnknown
			accounts[importedIndex] = previous
			accounts = append(accounts[:i], accounts[i+1:]...)
			merged++
			if i < importedIndex {
				importedIndex--
			}
			i = -1
		}
	}
	return accounts, merged
}

func accountFromRecord(token, source string, rec map[string]any) Account {
	typ := strings.TrimSpace(strAny(rec["type"], strAny(rec["plan_type"], "free")))
	if strings.EqualFold(typ, "codex") {
		typ = "free"
		source = "codex"
	}
	if typ == "" {
		typ = "free"
	}
	if strings.EqualFold(strAny(rec["export_type"], ""), "codex") {
		source = "codex"
	}
	if recordSource := strings.ToLower(strings.TrimSpace(strAny(rec["source_type"], ""))); recordSource == "web" || recordSource == "codex" {
		source = recordSource
	}
	status := strings.TrimSpace(strAny(rec["status"], "正常"))
	if status == "" {
		status = "正常"
	}
	now := nowISO()
	a := Account{AccessToken: token, Type: typ, SourceType: source, IsSignupToken: isSignupAccessToken(token), Status: status, Quota: intAny(rec["quota"], 0), Success: intAny(rec["success"], 0), Fail: intAny(rec["fail"], 0), CreatedAt: &now, ImageQuotaUnknown: boolAny(rec["image_quota_unknown"], false)}
	a.WindowSuccess = max(0, intAny(rec["window_success"], 0))
	a.WindowFail = max(0, intAny(rec["window_fail"], 0))
	a.WindowStartedAt = strAny(rec["window_started_at"], "")
	a.WindowResetAt = strAny(rec["window_reset_at"], "")
	a.Proxy = strings.TrimSpace(strAny(rec["proxy"], ""))
	a.GroupID = strings.TrimSpace(strAny(rec["group_id"], ""))
	a.ExportType = strings.TrimSpace(strAny(rec["export_type"], ""))
	if strings.EqualFold(strAny(rec["type"], ""), "codex") {
		a.ExportType = "codex"
	}
	if v := strings.TrimSpace(strAny(rec["created_at"], "")); v != "" {
		a.CreatedAt = &v
	}
	if v := strings.TrimSpace(strAny(rec["password"], "")); v != "" {
		a.Password = &v
		a.PasswordStatus = normalizePasswordStatus(strAny(rec["password_status"], ""))
	}
	if v := strings.TrimSpace(strAny(rec["email_password"], "")); v != "" {
		a.EmailPassword = &v
	}
	if raw, ok := rec["mailbox"].(map[string]any); ok {
		a.Mailbox = make(map[string]any, len(raw))
		for k, v := range raw {
			a.Mailbox[k] = v
		}
	}
	mailboxFields := []struct{ source, target string }{
		{"mailbox_provider", "provider"},
		{"mailbox_refresh_token", "refresh_token"},
		{"mailbox_client_id", "client_id"},
		{"mailbox_email", "login_email"},
		{"mailbox_login_email", "login_email"},
		{"imap_host", "imap_host"},
	}
	for _, field := range mailboxFields {
		if v := strings.TrimSpace(strAny(rec[field.source], "")); v != "" {
			if a.Mailbox == nil {
				a.Mailbox = map[string]any{}
			}
			a.Mailbox[field.target] = v
		}
	}
	if v := firstNonEmpty(strings.TrimSpace(strAny(rec["mailbox_password"], "")), ptrString(a.EmailPassword)); v != "" {
		if a.Mailbox == nil {
			a.Mailbox = map[string]any{}
		}
		a.Mailbox["password"] = v
	}
	if v := strings.TrimSpace(strAny(rec["email"], "")); v != "" {
		a.Email = &v
	}
	a.MFAStatus = strings.TrimSpace(strAny(rec["mfa_status"], ""))
	if v := strings.TrimSpace(strAny(rec["mfa_secret"], "")); v != "" {
		a.MFASecret = &v
	}
	if v := strings.TrimSpace(strAny(rec["user_id"], "")); v != "" {
		a.UserID = &v
	}
	if v := strings.TrimSpace(strAny(rec["refresh_token"], "")); v != "" {
		a.RefreshToken = &v
	}
	if v := strings.TrimSpace(strAny(rec["id_token"], "")); v != "" {
		a.IDToken = &v
	}
	if v := strings.TrimSpace(strAny(rec["account_id"], strAny(rec["chatgpt_account_id"], ""))); v != "" {
		a.AccountID = &v
	}
	if v := strings.TrimSpace(strAny(rec["client_id"], "")); v != "" {
		a.ClientID = &v
	}
	if v := strings.TrimSpace(strAny(rec["default_model_slug"], "")); v != "" {
		a.DefaultModelSlug = &v
	}
	if v := strings.TrimSpace(strAny(rec["restore_at"], "")); v != "" {
		a.RestoreAt = &v
	}
	if v := strings.TrimSpace(strAny(rec["rate_limited_at"], "")); v != "" {
		a.RateLimitedAt = &v
	}
	if v := strings.TrimSpace(strAny(rec["rate_limit_reset_at"], "")); v != "" {
		a.RateLimitResetAt = &v
	}
	if arr, ok := rec["limits_progress"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				a.LimitsProgress = append(a.LimitsProgress, m)
			}
		}
	}
	a.FP = accountFPFromRecord(rec)
	if a.InitialQuota < a.Quota {
		a.InitialQuota = a.Quota
	}
	return a
}
func accountFPFromRecord(rec map[string]any) map[string]string {
	fp := map[string]string{}
	if raw, ok := rec["fp"].(map[string]any); ok {
		for k, v := range raw {
			if s := strings.TrimSpace(strAny(v, "")); s != "" {
				fp[strings.ToLower(strings.TrimSpace(k))] = s
			}
		}
	}
	for _, key := range []string{"user-agent", "impersonate", "oai-device-id", "oai-session-id", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform"} {
		if s := strings.TrimSpace(strAny(rec[key], "")); s != "" {
			fp[key] = s
		}
	}
	if len(fp) == 0 {
		return nil
	}
	return fp
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A fresh token may be imported with the same email/password but no status
// metadata. Preserve the old account's successful password result across the
// subsequent email merge; a changed password or explicit status still wins.
func preserveImportedPasswordEvidence(accounts []Account, records map[string]map[string]any, finalTokens map[string]string) {
	type credential struct{ email, password string }
	accepted := map[credential]bool{}
	for _, account := range accounts {
		email, password := strings.ToLower(ptrString(account.Email)), ptrString(account.Password)
		if email != "" && password != "" && account.PasswordStatus == "confirmed" {
			accepted[credential{email, password}] = true
		}
	}
	imports := map[string]bool{}
	for token, record := range records {
		if _, explicit := record["password_status"]; !explicit {
			imports[firstNonEmpty(finalTokens[token], token)] = true
		}
	}
	for i := range accounts {
		if imports[accounts[i].AccessToken] && accepted[credential{strings.ToLower(ptrString(accounts[i].Email)), ptrString(accounts[i].Password)}] {
			accounts[i].PasswordStatus = "confirmed"
		}
	}
}

func mergeImportedAccount(dst *Account, src Account, rec map[string]any, explicitSource bool) {
	if _, explicit := rec["password_status"]; !explicit && src.Password != nil && ptrString(dst.Password) == ptrString(src.Password) {
		// An export without status metadata is not a new password attempt.
		src.PasswordStatus = ""
	}
	if _, ok := rec["group_id"]; ok {
		dst.GroupID = src.GroupID
	}
	if _, ok := rec["proxy"]; ok {
		dst.Proxy = src.Proxy
	}
	if strings.TrimSpace(strAny(rec["type"], strAny(rec["plan_type"], ""))) == "" {
		src.Type = ""
	}
	if strings.TrimSpace(strAny(rec["status"], "")) == "" {
		src.Status = ""
	}
	if !explicitSource && strings.TrimSpace(strAny(rec["source_type"], strAny(rec["export_type"], ""))) == "" && !strings.EqualFold(strAny(rec["type"], ""), "codex") {
		src.SourceType = ""
	}
	if _, ok := rec["quota"]; !ok {
		src.Quota = dst.Quota
		src.ImageQuotaUnknown = dst.ImageQuotaUnknown
	}
	mergeAccount(dst, src)
}

func mergeAccount(dst *Account, src Account) {
	if src.Proxy != "" {
		dst.Proxy = src.Proxy
	}
	if src.ExportType != "" {
		dst.ExportType = src.ExportType
	}
	if dst.CreatedAt == nil && src.CreatedAt != nil {
		dst.CreatedAt = src.CreatedAt
	}
	if src.Password != nil {
		if ptrString(dst.Password) != *src.Password {
			dst.PasswordStatus = "unverified"
		}
		dst.Password = src.Password
		if src.PasswordStatus != "" {
			dst.PasswordStatus = normalizePasswordStatus(src.PasswordStatus)
		}
	}
	if src.EmailPassword != nil {
		if dst.Password != nil && *dst.Password == *src.EmailPassword && src.Password == nil {
			dst.Password = nil
		}
		dst.EmailPassword = src.EmailPassword
	}
	if len(src.Mailbox) > 0 {
		if dst.Mailbox == nil {
			dst.Mailbox = map[string]any{}
		}
		for k, v := range src.Mailbox {
			dst.Mailbox[k] = v
		}
	}
	if src.Type != "" {
		dst.Type = src.Type
	}
	if src.TrialCheckedAt != "" {
		dst.TrialState = src.TrialState
		dst.TrialPromoID = src.TrialPromoID
		dst.TrialPromoLabel = src.TrialPromoLabel
		dst.TrialCheckedAt = src.TrialCheckedAt
	}
	if src.QuotaCheckedAt != "" {
		dst.QuotaCheckedAt = src.QuotaCheckedAt
	}
	if src.ImageQuotaBaseline != nil {
		dst.ImageQuotaBaseline = src.ImageQuotaBaseline
	}
	if src.SourceType != "" {
		dst.SourceType = src.SourceType
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if !src.ImageQuotaUnknown || src.Quota != 0 || len(src.LimitsProgress) > 0 {
		dst.Quota = src.Quota
	}
	if src.Email != nil {
		dst.Email = src.Email
	}
	if src.MFAStatus != "" {
		dst.MFAStatus = src.MFAStatus
	}
	if src.MFASecret != nil {
		dst.MFASecret = src.MFASecret
	}
	if src.UserID != nil {
		dst.UserID = src.UserID
	}
	if src.RefreshToken != nil {
		dst.RefreshToken = src.RefreshToken
	}
	if src.IDToken != nil {
		dst.IDToken = src.IDToken
	}
	if src.AccountID != nil {
		dst.AccountID = src.AccountID
	}
	if src.ClientID != nil {
		dst.ClientID = src.ClientID
	}
	if src.DefaultModelSlug != nil {
		dst.DefaultModelSlug = src.DefaultModelSlug
	}
	if src.RestoreAt != nil {
		dst.RestoreAt = src.RestoreAt
	}
	if src.RateLimitedAt != nil {
		dst.RateLimitedAt = src.RateLimitedAt
	}
	if src.RateLimitResetAt != nil {
		dst.RateLimitResetAt = src.RateLimitResetAt
	}
	if len(src.LimitsProgress) > 0 {
		dst.LimitsProgress = src.LimitsProgress
		dst.ImageQuotaUnknown = src.ImageQuotaUnknown
	}
	if src.Quota > 0 || src.RestoreAt != nil || src.DefaultModelSlug != nil {
		dst.ImageQuotaUnknown = src.ImageQuotaUnknown
	}
	if len(src.FP) > 0 {
		if dst.FP == nil {
			dst.FP = map[string]string{}
		}
		for k, v := range src.FP {
			if strings.TrimSpace(v) != "" {
				dst.FP[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
	}
	if dst.InitialQuota < dst.Quota {
		dst.InitialQuota = dst.Quota
	}
}

func mergeRefreshedAccount(dst *Account, info Account) {
	status := dst.Status
	baseline := refreshedImageQuotaBaseline(*dst, info)
	refreshAccountWindowStats(dst, info, time.Now())
	info.SourceType = ""
	mergeAccount(dst, info)
	dst.ImageQuotaBaseline = baseline
	if status == "异常" || status == "禁用" || status == "封号" {
		dst.Status = status
	} else if status == "限流" {
		if info.Status == "正常" && !info.ImageQuotaUnknown && info.Quota > 0 {
			dst.Status = "正常"
			dst.RestoreAt = info.RestoreAt
			dst.RateLimitedAt = nil
			dst.RateLimitResetAt = info.RateLimitResetAt
		} else {
			dst.Status = "限流"
		}
	} else if info.Status == "正常" {
		dst.ImageQuotaUnknown = info.ImageQuotaUnknown
		dst.RestoreAt = info.RestoreAt
		dst.RateLimitResetAt = info.RateLimitResetAt
	}
}

// The upstream exposes remaining quota, not the full allowance. Track an
// observed maximum within the same reset window for a reference percentage.
func refreshedImageQuotaBaseline(previous, info Account) *int {
	if info.ImageQuotaUnknown {
		return nil
	}
	baseline := max(0, info.Quota)
	previousReset, previousErr := parseAccountTime(ptrString(previous.RestoreAt))
	currentReset, currentErr := parseAccountTime(ptrString(info.RestoreAt))
	if previous.ImageQuotaBaseline != nil && previousErr == nil && currentErr == nil && previousReset.Equal(currentReset) {
		baseline = max(baseline, *previous.ImageQuotaBaseline)
	}
	return &baseline
}

func (s *Server) refreshAccountInfos(parent context.Context, tokens []string) (int, []map[string]any) {
	refreshed, errs, _ := s.refreshAccountInfosWithFinalTokens(parent, tokens)
	return refreshed, errs
}

func (s *Server) refreshAccountInfosWithFinalTokens(parent context.Context, tokens []string) (int, []map[string]any, map[string]string) {
	want := map[string]bool{}
	for _, t := range tokens {
		if strings.TrimSpace(t) != "" {
			want[strings.TrimSpace(t)] = true
		}
	}
	selected := []Account{}
	for _, a := range s.store.LoadAccounts() {
		if len(tokens) == 0 && isAbnormalAccountStatus(a.Status) {
			continue
		}
		if len(tokens) > 0 && !want[a.AccessToken] {
			continue
		}
		selected = append(selected, a)
		delete(want, a.AccessToken)
	}
	finalTokens := map[string]string{}
	var finalTokensMu sync.Mutex
	results := runAccountRefreshBatch(parent, selected, func(ctx context.Context, account Account) error {
		return s.refreshAccountInfoWithFinalToken(ctx, account, func(token string) {
			finalTokensMu.Lock()
			finalTokens[account.AccessToken] = token
			finalTokensMu.Unlock()
		})
	})
	refreshed := 0
	errs := []map[string]any{}
	for token := range want {
		errs = append(errs, map[string]any{"token": token, "error": "account not found"})
	}
	for i, err := range results {
		if err == nil {
			refreshed++
		} else {
			errs = append(errs, map[string]any{"token": selected[i].AccessToken, "error": err.Error()})
		}
	}
	return refreshed, errs, finalTokens
}

func runAccountRefreshBatch(parent context.Context, accounts []Account, refresh func(context.Context, Account) error) []error {
	return runAccountRefreshBatchReport(parent, accounts, refresh, nil)
}

func runAccountRefreshBatchReport(parent context.Context, accounts []Account, refresh func(context.Context, Account) error, report func(int, error)) []error {
	results := make([]error, len(accounts))
	if len(accounts) == 0 {
		return results
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(accounts), 8) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if err := parent.Err(); err != nil {
					results[i] = err
					if report != nil {
						report(i, err)
					}
					continue
				}
				timeout := 45 * time.Second
				if ptrString(accounts[i].Password) != "" || ptrString(accounts[i].EmailPassword) != "" ||
					strings.HasSuffix(strings.ToLower(ptrString(accounts[i].Email)), "@icloud.com") ||
					(accounts[i].Mailbox != nil && (strAny(accounts[i].Mailbox["password"], "") != "" ||
						strAny(accounts[i].Mailbox["refresh_token"], "") != "")) {
					timeout = 180 * time.Second
				}
				ctx, cancel := context.WithTimeout(parent, timeout)
				results[i] = refresh(ctx, accounts[i])
				cancel()
				if report != nil {
					report(i, results[i])
				}
			}
		}()
	}
	for i := range accounts {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return results
}

func (s *Server) refreshAccountInfo(ctx context.Context, a Account) error {
	return s.refreshAccountInfoWithFinalToken(ctx, a, nil)
}

func (s *Server) refreshAccountInfoWithFinalToken(ctx context.Context, a Account, onToken func(string)) error {
	if onToken != nil {
		defer func() { onToken(a.AccessToken) }()
	}
	release, err := s.acquireAccountRefreshSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	var preflightErr error
	if ptrString(a.RefreshToken) != "" && (accessTokenNeedsRefresh(a.AccessToken) || isSignupAccessToken(a.AccessToken)) {
		if refreshed, err := s.refreshOAuthAccount(ctx, a.AccessToken); err == nil {
			a = refreshed
		} else {
			preflightErr = err
		}
	}
	client, err := s.newAccountLoginClient(ctx, a)
	if err != nil {
		return err
	}
	info, err := client.GetUserInfo(ctx)
	canRelogin := ptrString(a.Password) != "" || s.canReadAccountEmailCode(a)
	if isInvalidTokenErrorText(err) && ptrString(a.RefreshToken) != "" {
		if preflightErr != nil && !terminalOAuthRefreshError(preflightErr) && !canRelogin {
			return fmt.Errorf("oauth refresh after account authentication failure: %w", preflightErr)
		}
		if preflightErr == nil {
			refreshed, refreshErr := s.refreshOAuthAccount(ctx, a.AccessToken)
			if refreshErr != nil && !terminalOAuthRefreshError(refreshErr) && !canRelogin {
				return fmt.Errorf("oauth refresh after account authentication failure: %w", refreshErr)
			}
			if refreshErr == nil {
				a = refreshed
				client, err = s.newAccountLoginClient(ctx, a)
				if err == nil {
					info, err = client.GetUserInfo(ctx)
				}
			}
		}
	}
	if isInvalidTokenErrorText(err) && canRelogin {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		oldToken := a.AccessToken
		relogged, loginErr := s.reLoginAfterRefreshAuthFailure(ctx, a, err)
		if loginErr != nil {
			return fmt.Errorf("登录令牌已失效，重新授权失败: %w", loginErr)
		}
		if persistErr := s.persistReLoginAccount(oldToken, relogged); persistErr != nil {
			return persistErr
		}
		a = relogged
		client, err = s.newAccountLoginClient(ctx, a)
		if err == nil {
			info, err = client.GetUserInfo(ctx)
		}
	}
	if err == nil {
		return s.applyAccountInfoRefresh(a, info)
	}
	if isInvalidTokenErrorText(err) || isAccountDeactivatedError(err) {
		s.markAccountRefreshAuthFailure(a, err)
	}
	return err
}

func (s *Server) reLoginAfterRefreshAuthFailure(ctx context.Context, a Account, authErr error) (Account, error) {
	var next Account
	var err error
	if ctx.Err() != nil {
		return Account{}, ctx.Err()
	}
	if ptrString(a.Password) != "" {
		next, err = s.reLoginAccountWithPasswordSecret(ctx, a, ptrString(a.MFASecret))
	} else {
		next, err = s.reLoginAccountWithEmailCode(ctx, a)
	}
	if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		// A failed reauthorization can prove deactivation more precisely than
		// the original token error. Network/login failures retain that 401.
		failure := authErr
		if isAccountDeactivatedError(err) {
			failure = err
		}
		s.markAccountRefreshAuthFailure(a, failure)
	}
	return next, err
}

func (s *Server) markAccountRefreshAuthFailure(observed Account, err error) {
	_ = s.store.UpdateAccounts(func(current []Account) []Account {
		for i := range current {
			if current[i].AccessToken == observed.AccessToken && ptrString(current[i].RefreshToken) == ptrString(observed.RefreshToken) {
				applyAccountFailureState(&current[i], err, nowISO())
				break
			}
		}
		return current
	})
}

func (s *Server) acquireAccountRefreshSlot(ctx context.Context) (func(), error) {
	if s.accountPool == nil || s.accountPool.refreshSlots == nil {
		return func() {}, nil
	}
	select {
	case s.accountPool.refreshSlots <- struct{}{}:
		return func() { <-s.accountPool.refreshSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) applyAccountInfoRefresh(observed, info Account) error {
	updated := false
	stale := false
	err := s.store.UpdateAccounts(func(current []Account) []Account {
		for i := range current {
			if current[i].AccessToken != observed.AccessToken {
				continue
			}
			if current[i].Success != observed.Success || current[i].Fail != observed.Fail || current[i].Status != observed.Status ||
				current[i].Quota != observed.Quota || current[i].ImageQuotaUnknown != observed.ImageQuotaUnknown ||
				current[i].QuotaCheckedAt != observed.QuotaCheckedAt ||
				ptrString(current[i].RefreshToken) != ptrString(observed.RefreshToken) ||
				ptrString(current[i].RateLimitedAt) != ptrString(observed.RateLimitedAt) ||
				ptrString(current[i].RateLimitResetAt) != ptrString(observed.RateLimitResetAt) ||
				ptrString(current[i].RestoreAt) != ptrString(observed.RestoreAt) {
				stale = true
				break
			}
			mergeRefreshedAccount(&current[i], info)
			updated = true
			break
		}
		return current
	})
	if err != nil {
		return err
	}
	if stale {
		return errors.New("account state changed during refresh")
	}
	if !updated {
		return errors.New("account credentials changed during refresh")
	}
	return nil
}

func (s *Server) handleAccountsRefresh(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var body struct {
		AccessTokens []string `json:"access_tokens"`
	}
	if !readBody(w, r, &body) {
		return
	}
	want := map[string]bool{}
	for _, t := range body.AccessTokens {
		if strings.TrimSpace(t) != "" {
			want[strings.TrimSpace(t)] = true
		}
	}
	if len(body.AccessTokens) > 0 && len(want) == 0 {
		writeErr(w, http.StatusBadRequest, "no valid accounts selected")
		return
	}
	var tokens []string
	if len(want) > 0 {
		for token := range want {
			tokens = append(tokens, token)
		}
	}
	refreshed, errs := s.refreshAccountInfos(r.Context(), tokens)
	writeJSON(w, 200, map[string]any{"refreshed": refreshed, "errors": errs, "items": s.store.LoadAccounts()})
}
func (s *Server) handleAccountsUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body map[string]any
	if !readBody(w, r, &body) {
		return
	}
	if rawTokens, ok := body["access_tokens"]; ok {
		values, ok := rawTokens.([]any)
		if !ok || len(values) == 0 {
			writeErr(w, http.StatusBadRequest, "access_tokens is required")
			return
		}
		status := strings.TrimSpace(strAny(body["status"], ""))
		_, hasStatus := body["status"]
		if hasStatus && status != "正常" && status != "禁用" {
			writeErr(w, http.StatusBadRequest, "status must be 正常 or 禁用")
			return
		}
		groupID := ""
		if raw, ok := body["group_id"]; ok {
			var valid bool
			groupID, valid = raw.(string)
			if !valid {
				writeErr(w, http.StatusBadRequest, "group_id must be a string")
				return
			}
			groupID = strings.TrimSpace(groupID)
			if err := s.store.ValidateAccountGroupID(groupID); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		} else if !hasStatus {
			writeErr(w, http.StatusBadRequest, "status or group_id is required")
			return
		}
		targets := make(map[string]bool, len(values))
		for _, value := range values {
			token, ok := value.(string)
			if !ok || strings.TrimSpace(token) == "" {
				writeErr(w, http.StatusBadRequest, "access_tokens contains an invalid token")
				return
			}
			targets[strings.TrimSpace(token)] = true
		}
		updated := 0
		recovery := s.registerRecoveryEmailSet()
		if err := s.store.UpdateAccounts(func(accounts []Account) []Account {
			for i := range accounts {
				if targets[accounts[i].AccessToken] {
					if hasStatus {
						accounts[i].Status = status
					}
					if _, ok := body["group_id"]; ok {
						accounts[i].GroupID = groupID
					}
					updated++
				}
			}
			return markIncompleteRegisterAccounts(accounts, recovery)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "update accounts failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "items": s.store.LoadAccounts()})
		return
	}
	token := strings.TrimSpace(strAny(body["access_token"], ""))
	if token == "" {
		writeErr(w, 400, "access_token is required")
		return
	}
	if raw, ok := body["proxy"]; ok {
		assignment, valid := raw.(string)
		if !valid {
			writeErr(w, http.StatusBadRequest, "proxy must be a string")
			return
		}
		normalized, err := s.store.normalizeAccountProxyAssignment(assignment)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "账号代理配置无效: "+err.Error())
			return
		}
		body["proxy"] = normalized
	}
	if raw, ok := body["group_id"]; ok {
		groupID, valid := raw.(string)
		if !valid {
			writeErr(w, http.StatusBadRequest, "group_id must be a string")
			return
		}
		groupID = strings.TrimSpace(groupID)
		if err := s.store.ValidateAccountGroupID(groupID); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		body["group_id"] = groupID
	}
	if raw, ok := body["password"]; ok {
		if _, valid := raw.(string); !valid {
			writeErr(w, http.StatusBadRequest, "password must be a string")
			return
		}
	}
	if raw, ok := body["mfa_secret"]; ok {
		secret, valid := raw.(string)
		if !valid {
			writeErr(w, http.StatusBadRequest, "mfa_secret must be a string")
			return
		}
		secret = strings.ToUpper(strings.Join(strings.Fields(secret), ""))
		if secret != "" {
			if _, err := registerTOTP(secret, time.Now()); err != nil {
				writeErr(w, http.StatusBadRequest, "2FA 密钥格式无效")
				return
			}
		}
		body["mfa_secret"] = secret
	}
	var updated *Account
	recovery := s.registerRecoveryEmailSet()
	if err := s.store.UpdateAccounts(func(accounts []Account) []Account {
		for i, a := range accounts {
			if a.AccessToken != token {
				continue
			}
			if v, ok := body["type"]; ok {
				a.Type = strAny(v, a.Type)
			}
			if v, ok := body["status"]; ok {
				a.Status = strAny(v, a.Status)
			}
			if v, ok := body["quota"]; ok {
				a.Quota = intAny(v, a.Quota)
			}
			if v, ok := body["proxy"]; ok {
				a.Proxy = v.(string)
			}
			if v, ok := body["group_id"]; ok {
				a.GroupID = v.(string)
			}
			if v, ok := body["password"]; ok {
				password := v.(string)
				if ptrString(a.Password) != password {
					a.PasswordStatus = "unverified"
				}
				a.Password = &password
			}
			if v, ok := body["mfa_secret"]; ok {
				secret := v.(string)
				if secret == "" {
					a.MFASecret = nil
					a.MFAStatus = ""
				} else {
					a.MFASecret = &secret
					a.MFAStatus = "enabled"
				}
			}
			a = markIncompleteRegisterAccounts([]Account{a}, recovery)[0]
			accounts[i] = a
			updated = &a
			break
		}
		return accounts
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新账号失败: "+err.Error())
		return
	}
	if updated != nil {
		s.clearCompletedRegisterRecovery([]Account{*updated})
		writeJSON(w, 200, map[string]any{"item": updated, "items": s.store.LoadAccounts()})
		return
	}
	writeErr(w, 404, "account not found")
}
