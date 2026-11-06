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

type accountRefreshFailure struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

type accountRefreshProgress struct {
	ID        string                  `json:"progress_id"`
	Total     int                     `json:"total"`
	Processed int                     `json:"processed"`
	Refreshed int                     `json:"refreshed"`
	Failed    int                     `json:"failed"`
	Done      bool                    `json:"done"`
	Errors    []accountRefreshFailure `json:"errors"`
	updatedAt time.Time
}

type accountRefreshJobs struct {
	mu    sync.Mutex
	items map[string]*accountRefreshProgress
}

func (j *accountRefreshJobs) create(total int) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.items == nil {
		j.items = make(map[string]*accountRefreshProgress)
	}
	now := time.Now()
	for id, progress := range j.items {
		if progress.Done && now.Sub(progress.updatedAt) >= 10*time.Minute {
			delete(j.items, id)
		}
	}
	if len(j.items) >= 64 {
		return "", errors.New("too many account refresh jobs")
	}
	id := uuid4()
	j.items[id] = &accountRefreshProgress{ID: id, Total: total, Errors: []accountRefreshFailure{}, updatedAt: now}
	return id, nil
}

func (j *accountRefreshJobs) record(id string, failure *accountRefreshFailure) {
	j.mu.Lock()
	defer j.mu.Unlock()
	progress := j.items[id]
	if progress == nil || progress.Done {
		return
	}
	progress.Processed++
	if failure == nil {
		progress.Refreshed++
	} else {
		progress.Failed++
		if len(progress.Errors) < 100 {
			progress.Errors = append(progress.Errors, *failure)
		}
	}
	progress.updatedAt = time.Now()
}

func (j *accountRefreshJobs) finish(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if progress := j.items[id]; progress != nil {
		progress.Done = true
		progress.updatedAt = time.Now()
	}
}

func (j *accountRefreshJobs) get(id string) (accountRefreshProgress, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	progress := j.items[id]
	if progress == nil {
		return accountRefreshProgress{}, false
	}
	if progress.Done && time.Since(progress.updatedAt) >= 10*time.Minute {
		delete(j.items, id)
		return accountRefreshProgress{}, false
	}
	copy := *progress
	copy.Errors = append([]accountRefreshFailure{}, progress.Errors...)
	return copy, true
}

func (s *Server) handleAccountsRefreshStart(w http.ResponseWriter, r *http.Request) {
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
	selected, missing := selectAccountsForRefresh(s.store.LoadAccounts(), body.AccessTokens)
	if len(selected) == 0 && missing == 0 {
		writeErr(w, http.StatusBadRequest, "no accounts to refresh")
		return
	}
	id, err := s.accountRefreshJobs.create(len(selected) + missing)
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	for range missing {
		s.accountRefreshJobs.record(id, &accountRefreshFailure{Account: "已删除账号", Error: "account not found"})
	}
	ctx := context.WithoutCancel(r.Context())
	go s.runAccountRefreshJob(ctx, id, selected)
	writeJSON(w, http.StatusAccepted, map[string]any{"progress_id": id, "total": len(selected) + missing})
}

func selectAccountsForRefresh(accounts []Account, tokens []string) ([]Account, int) {
	want := map[string]bool{}
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			want[token] = true
		}
	}
	all := len(tokens) == 0
	selected := []Account{}
	for _, account := range accounts {
		if all && isAbnormalAccountStatus(account.Status) {
			continue
		}
		if all || want[account.AccessToken] {
			selected = append(selected, account)
			delete(want, account.AccessToken)
		}
	}
	return selected, len(want)
}

func (s *Server) runAccountRefreshJob(ctx context.Context, id string, accounts []Account) {
	defer s.accountRefreshJobs.finish(id)
	runAccountRefreshBatchReport(ctx, accounts, s.refreshAccountInfo, func(index int, err error) {
		if err == nil {
			s.accountRefreshJobs.record(id, nil)
			return
		}
		account := accounts[index]
		label := ptrString(account.Email)
		if label == "" {
			label = fmt.Sprintf("账号 #%d", index+1)
		}
		message := err.Error()
		for _, secret := range []string{account.AccessToken, ptrString(account.RefreshToken), ptrString(account.IDToken), ptrString(account.Password), account.Proxy, s.cfg.Proxy} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		s.accountRefreshJobs.record(id, &accountRefreshFailure{Account: label, Error: truncateText(message, 500)})
	})
}

func (s *Server) handleAccountsRefreshProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/refresh/progress/")
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, http.StatusBadRequest, "invalid progress id")
		return
	}
	progress, ok := s.accountRefreshJobs.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "progress not found")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}
