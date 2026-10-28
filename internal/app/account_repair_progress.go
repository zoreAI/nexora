package app

import (
	"context"
	"net/http"
	"strings"
	"time"
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
		AccessTokens []string `json:"access_tokens"`
	}
	if !readBody(w, r, &body) {
		return
	}
	selected := selectAbnormalAccounts(s.store.LoadAccounts(), body.AccessTokens)
	if len(selected) == 0 {
		writeErr(w, http.StatusBadRequest, "请选择异常账号")
		return
	}
	id, err := s.accountRepairJobs.create(len(selected))
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	go s.runAccountRepairJob(context.WithoutCancel(r.Context()), id, selected)
	writeJSON(w, http.StatusAccepted, map[string]any{"progress_id": id, "total": len(selected)})
}

func (s *Server) runAccountRepairJob(ctx context.Context, id string, accounts []Account) {
	defer s.accountRepairJobs.finish(id)
	for _, account := range accounts {
		current := Account{}
		for _, item := range s.store.LoadAccounts() {
			if item.AccessToken == account.AccessToken {
				current = item
				break
			}
		}
		label := firstNonEmpty(ptrString(account.Email), "账号")
		if current.AccessToken == "" || current.Status != "异常" {
			s.accountRepairJobs.record(id, &accountRefreshFailure{Account: label, Error: "账号已删除或状态已变化"})
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
		repaired, skipped, errs, skippedItems := s.reLoginAccounts(itemCtx, []string{current.AccessToken})
		cancel()
		if repaired > 0 {
			s.accountRepairJobs.record(id, nil)
			continue
		}
		message := "账号缺少可用的重新授权方式"
		if skipped > 0 && len(skippedItems) > 0 {
			message = strAny(skippedItems[0]["reason"], message)
		}
		if len(errs) > 0 {
			message = strAny(errs[0]["error"], message)
		}
		for _, secret := range []string{current.AccessToken, ptrString(current.Password), ptrString(current.MFASecret), ptrString(current.RefreshToken), ptrString(current.IDToken), current.Proxy, s.cfg.Proxy} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		s.accountRepairJobs.record(id, &accountRefreshFailure{Account: label, Error: truncateText(message, 500)})
	}
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
