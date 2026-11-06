package app

import (
	"context"
	"net/http"
	"strings"
)

func selectAbnormalAccounts(accounts []Account, tokens []string) []Account {
	want := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			want[token] = true
		}
	}
	selected := make([]Account, 0, len(want))
	for _, account := range accounts {
		if account.Status == "异常" && want[account.AccessToken] {
			selected = append(selected, account)
		}
	}
	return selected
}

func (s *Server) handleAccountsRepairStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		AccessTokens   []string `json:"access_tokens"`
		RecoveryEmails []string `json:"recovery_emails"`
		RetryBlocked   bool     `json:"retry_blocked"`
	}
	if !readBody(w, r, &body) {
		return
	}
	selected := selectAbnormalAccounts(s.store.LoadAccounts(), body.AccessTokens)
	pending := s.selectPendingRegisterEmails(body.RecoveryEmails, body.RetryBlocked)
	skipped := len(s.selectPendingRegisterEmails(body.RecoveryEmails, true)) - len(pending)
	if len(selected)+len(pending) == 0 {
		if skipped > 0 {
			writeErr(w, http.StatusBadRequest, "所选待接续账号需要人工处理，请查看号池中的原因，处理后单条重新检查")
			return
		}
		writeErr(w, http.StatusBadRequest, "请选择异常账号")
		return
	}
	targets := accountRepairTargets(selected, pending)
	id, err := s.accountRepairJobs.create(len(targets))
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	go s.runAccountRepairJob(context.WithoutCancel(r.Context()), id, selected, pending)
	writeJSON(w, http.StatusAccepted, map[string]any{"progress_id": id, "total": len(targets), "skipped": skipped})
}

func (s *Server) runAccountRepairJob(ctx context.Context, id string, accounts []Account, pendingEmails []string) {
	defer s.accountRepairJobs.finish(id)
	if err := s.ensureAccountRepairMihomo(ctx); err != nil {
		for _, target := range accountRepairTargets(accounts, pendingEmails) {
			s.accountRepairJobs.record(id, &accountRefreshFailure{Account: firstNonEmpty(target.email, "账号"), Error: "准备 Mihomo 修复代理失败: " + err.Error()})
		}
		return
	}
	mail := RegisterMailConfig{}
	if s.register != nil {
		mail = s.register.snapshot().Mail
	}
	runAccountRepairTargets(ctx, accountRepairTargets(accounts, pendingEmails), s.accountRepairThreads(), accountRepairTimeout(mail), s.repairAccountTarget, nil, func(item accountRepairTarget, _ int, err error) {
		if err == nil {
			s.accountRepairJobs.record(id, nil)
			return
		}
		message := err.Error()
		for _, secret := range []string{item.account.AccessToken, ptrString(item.account.Password), ptrString(item.account.MFASecret), ptrString(item.account.RefreshToken), ptrString(item.account.IDToken), item.account.Proxy, s.cfg.Proxy} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		s.accountRepairJobs.record(id, &accountRefreshFailure{Account: firstNonEmpty(item.email, "账号"), Error: truncateText(message, 500)})
	})
}

func (s *Server) handleAccountsRepairProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/repair/progress/")
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, http.StatusBadRequest, "invalid progress id")
		return
	}
	progress, ok := s.accountRepairJobs.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "progress not found")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}
