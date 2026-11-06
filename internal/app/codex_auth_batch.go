package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CodexAuthCredential is deliberately independent from Account. It is used
// for one-shot OAuth acquisition and is not persisted in the account pool.
type CodexAuthCredential struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	MFASecret string `json:"mfa_secret"`
	Proxy     string `json:"proxy,omitempty"`
	Name      string `json:"name,omitempty"`
}

type authBatchMode string

const (
	authBatchModeCodex authBatchMode = "codex"
	authBatchModeWeb   authBatchMode = "web"
)

type codexAuthExport struct {
	AuthMode         string            `json:"auth_mode"`
	Name             string            `json:"name,omitempty"`
	Email            string            `json:"email,omitempty"`
	PlanType         string            `json:"plan_type,omitempty"`
	ChatGPTAccountID string            `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID    string            `json:"chatgpt_user_id,omitempty"`
	Tokens           map[string]string `json:"tokens"`
	Credentials      map[string]string `json:"credentials"`
}

type codexAuthBatchItem struct {
	Index  int              `json:"index"`
	Email  string           `json:"email"`
	Name   string           `json:"name,omitempty"`
	Status string           `json:"status"`
	Error  string           `json:"error,omitempty"`
	Export *codexAuthExport `json:"-"`
}

type codexAuthBatchProgress struct {
	ID        string               `json:"progress_id"`
	Mode      string               `json:"mode"`
	Total     int                  `json:"total"`
	Processed int                  `json:"processed"`
	Succeeded int                  `json:"succeeded"`
	Failed    int                  `json:"failed"`
	Done      bool                 `json:"done"`
	Items     []codexAuthBatchItem `json:"items"`
	updatedAt time.Time
}

type codexAuthBatchJobs struct {
	mu    sync.Mutex
	items map[string]*codexAuthBatchProgress
}

func (j *codexAuthBatchJobs) create(items []CodexAuthCredential, mode authBatchMode) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.items == nil {
		j.items = map[string]*codexAuthBatchProgress{}
	}
	now := time.Now()
	for id, item := range j.items {
		if item.Done && now.Sub(item.updatedAt) >= 30*time.Minute {
			delete(j.items, id)
		}
	}
	if len(j.items) >= 32 {
		return "", errors.New("too many Codex authorization jobs")
	}
	id := uuid4()
	progress := &codexAuthBatchProgress{ID: id, Mode: string(mode), Total: len(items), Items: make([]codexAuthBatchItem, len(items)), updatedAt: now}
	for i, item := range items {
		progress.Items[i] = codexAuthBatchItem{Index: i + 1, Email: strings.TrimSpace(item.Email), Name: strings.TrimSpace(item.Name), Status: "queued"}
	}
	j.items[id] = progress
	return id, nil
}

func (j *codexAuthBatchJobs) update(id string, index int, item codexAuthBatchItem) {
	j.mu.Lock()
	defer j.mu.Unlock()
	p := j.items[id]
	if p == nil || p.Done || index < 0 || index >= len(p.Items) {
		return
	}
	p.Items[index] = item
	p.Processed++
	if item.Status == "success" {
		p.Succeeded++
	} else {
		p.Failed++
	}
	p.updatedAt = time.Now()
}

func (j *codexAuthBatchJobs) finish(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if p := j.items[id]; p != nil {
		p.Done = true
		p.updatedAt = time.Now()
	}
}

func (j *codexAuthBatchJobs) get(id string) (codexAuthBatchProgress, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	p := j.items[id]
	if p == nil || (p.Done && time.Since(p.updatedAt) >= 30*time.Minute) {
		return codexAuthBatchProgress{}, false
	}
	copy := *p
	copy.Items = append([]codexAuthBatchItem(nil), p.Items...)
	for i := range copy.Items {
		copy.Items[i].Export = nil
	}
	return copy, true
}

func (s *Server) handleCodexAuthBatchStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Accounts    []CodexAuthCredential `json:"accounts"`
		Concurrency int                   `json:"concurrency"`
		Mode        string                `json:"mode"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if len(body.Accounts) == 0 || len(body.Accounts) > 500 {
		writeErr(w, http.StatusBadRequest, "accounts must contain 1-500 items")
		return
	}
	for i := range body.Accounts {
		body.Accounts[i].Email = strings.TrimSpace(body.Accounts[i].Email)
		if body.Accounts[i].Email == "" || strings.TrimSpace(body.Accounts[i].Password) == "" || strings.TrimSpace(body.Accounts[i].MFASecret) == "" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d] requires email, password and mfa_secret", i))
			return
		}
	}
	mode := authBatchMode(strings.ToLower(strings.TrimSpace(body.Mode)))
	if mode == "" {
		mode = authBatchModeCodex
	}
	if mode != authBatchModeCodex && mode != authBatchModeWeb {
		writeErr(w, http.StatusBadRequest, "mode must be codex or web")
		return
	}
	id, err := s.codexAuthJobs.create(body.Accounts, mode)
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	concurrency := body.Concurrency
	if concurrency <= 0 {
		concurrency = 3
	}
	if concurrency > 8 {
		concurrency = 8
	}
	go s.runCodexAuthBatch(context.WithoutCancel(r.Context()), id, body.Accounts, concurrency, mode)
	writeJSON(w, http.StatusAccepted, map[string]any{"progress_id": id, "total": len(body.Accounts)})
}

func (s *Server) handleCodexAuthBatchProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/codex-auth/batch/")
	id = strings.TrimSuffix(id, "/export")
	if strings.Contains(id, "/") || id == "" {
		writeErr(w, http.StatusBadRequest, "invalid progress id")
		return
	}
	p, ok := s.codexAuthJobs.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "progress not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleCodexAuthBatchExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/codex-auth/batch/"), "/export")
	p, ok := s.codexAuthJobs.getRaw(id)
	if !ok || !p.Done {
		writeErr(w, http.StatusConflict, "batch is not finished")
		return
	}
	accounts := make([]codexAuthExport, 0, p.Succeeded)
	for _, item := range p.Items {
		if item.Export != nil {
			accounts = append(accounts, *item.Export)
		}
	}
	w.Header().Set("Content-Disposition", `attachment; filename="codex-sub2api.json"`)
	// Sub2API's Codex session importer accepts a JSON array as its content.
	writeJSON(w, http.StatusOK, accounts)
}

func (j *codexAuthBatchJobs) getRaw(id string) (codexAuthBatchProgress, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	p := j.items[id]
	if p == nil {
		return codexAuthBatchProgress{}, false
	}
	copy := *p
	copy.Items = append([]codexAuthBatchItem(nil), p.Items...)
	return copy, true
}

func (s *Server) runCodexAuthBatch(ctx context.Context, id string, accounts []CodexAuthCredential, concurrency int, mode authBatchMode) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for n := 0; n < concurrency; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				item := accounts[i]
				result := codexAuthBatchItem{Index: i + 1, Email: item.Email, Name: item.Name, Status: "failed"}
				export, err := s.loginStandaloneCodex(ctx, item, mode)
				if err == nil {
					result.Status = "success"
					result.Export = &export
				} else {
					result.Error = redactCodexAuthError(err, item)
				}
				s.codexAuthJobs.update(id, i, result)
			}
		}()
	}
	for i := range accounts {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	s.codexAuthJobs.finish(id)
}

func redactCodexAuthError(err error, item CodexAuthCredential) string {
	if isAccountDeactivatedError(err) {
		return "403 account_deactivated，账号已被停用"
	}
	message := err.Error()
	for _, secret := range []string{item.Password, item.MFASecret} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return truncateText(message, 500)
}

func (s *Server) loginStandaloneCodex(ctx context.Context, item CodexAuthCredential, mode authBatchMode) (codexAuthExport, error) {
	proxy := strings.TrimSpace(item.Proxy)
	if proxy == "" {
		proxy = strings.TrimSpace(s.cfg.Proxy)
	}
	session, err := newRegisterSession(proxy, 60, s.ensureCurlImpersonateBinary)
	if err != nil {
		return codexAuthExport{}, err
	}
	defer session.close()
	if mode == authBatchModeCodex {
		session.oauthClientID = codexOAuthClientID
	} else {
		session.oauthClientID = webOAuthClientID
	}
	tokens, err := session.loginWithPassword(ctx, item.Email, item.Password, item.MFASecret)
	if err != nil {
		return codexAuthExport{}, err
	}
	access := strings.TrimSpace(strAny(tokens["access_token"], ""))
	if access == "" {
		return codexAuthExport{}, errors.New("OAuth response missing access_token")
	}
	refresh := strings.TrimSpace(strAny(tokens["refresh_token"], ""))
	idToken := strings.TrimSpace(strAny(tokens["id_token"], ""))
	meta := codexTokenMetadata(access, idToken)
	if meta["chatgpt_account_id"] == "" {
		meta["chatgpt_account_id"] = chatGPTAccountID(access)
	}
	clientID := webOAuthClientID
	if mode == authBatchModeCodex {
		clientID = codexOAuthClientID
	}
	credentials := map[string]string{"access_token": access, "client_id": clientID, "auth_mode": string(mode)}
	if expiresIn := intAny(tokens["expires_in"], 0); expiresIn > 0 {
		credentials["expires_at"] = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	}
	if refresh != "" {
		credentials["refresh_token"] = refresh
	}
	if idToken != "" {
		credentials["id_token"] = idToken
	}
	for _, key := range []string{"chatgpt_account_id", "chatgpt_user_id", "plan_type", "email"} {
		if meta[key] != "" {
			credentials[key] = meta[key]
		}
	}
	if meta["expires_at"] != "" {
		credentials["expires_at"] = meta["expires_at"]
	}
	return codexAuthExport{AuthMode: string(mode), Name: firstNonEmpty(item.Name, item.Email, meta["chatgpt_account_id"]), Email: firstNonEmpty(meta["email"], item.Email), PlanType: meta["plan_type"], ChatGPTAccountID: meta["chatgpt_account_id"], ChatGPTUserID: meta["chatgpt_user_id"], Tokens: map[string]string{"access_token": access, "refresh_token": refresh, "id_token": idToken}, Credentials: credentials}, nil
}

func codexTokenMetadata(accessToken, idToken string) map[string]string {
	out := map[string]string{}
	for _, token := range []string{idToken, accessToken} {
		parts := strings.Split(token, ".")
		if len(parts) < 2 {
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims map[string]any
		if json.Unmarshal(payload, &claims) != nil {
			continue
		}
		if out["email"] == "" {
			out["email"] = strAny(claims["email"], "")
		}
		if out["chatgpt_user_id"] == "" {
			out["chatgpt_user_id"] = strAny(claims["sub"], "")
		}
		if exp := intAny(claims["exp"], 0); exp > 0 && out["expires_at"] == "" {
			out["expires_at"] = time.Unix(int64(exp), 0).UTC().Format(time.RFC3339)
		}
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			for dst, src := range map[string]string{"chatgpt_account_id": "chatgpt_account_id", "chatgpt_user_id": "chatgpt_user_id", "plan_type": "chatgpt_plan_type"} {
				if out[dst] == "" {
					out[dst] = strAny(auth[src], "")
				}
			}
		}
	}
	return out
}
