package app

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

type accountPool struct {
	mu           sync.Mutex
	index        int
	inflight     map[string]int
	refreshSlots chan struct{}
	cfg          *Config
}

func newAccountPool(cfg *Config) *accountPool {
	return &accountPool{inflight: make(map[string]int), refreshSlots: make(chan struct{}, 8), cfg: cfg}
}

func ptrString(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

func isSignupAccessToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Auth struct {
			IsSignup bool `json:"is_signup"`
		} `json:"https://api.openai.com/auth"`
	}
	return json.Unmarshal(payload, &claims) == nil && claims.Auth.IsSignup
}

func parseAccountTime(value string) (time.Time, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return time.Time{}, errors.New("empty time")
	}
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	if strings.HasSuffix(text, "Z") {
		return time.Parse(time.RFC3339Nano, text)
	}
	return time.Parse(time.RFC3339Nano, text)
}

func (p *accountPool) pickToken(accounts []Account, needCodex bool) (string, error) {
	return p.pickTokenExcluding(accounts, needCodex, "", nil)
}

func (p *accountPool) pickTokenExcluding(accounts []Account, needCodex bool, codexPlanType string, excluded map[string]bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	maxConc := p.cfg.ImageAccountConcurrency
	if maxConc < 1 {
		maxConc = 3
	}
	premium := map[string]bool{"plus": true, "pro": true, "team": true}
	now := time.Now().UTC()
	for signupPass := 0; signupPass < 2; signupPass++ {
		for offset := 0; offset < len(accounts); offset++ {
			idx := (p.index + offset) % len(accounts)
			a := accounts[idx]
			if a.AccessToken == "" || excluded[a.AccessToken] || isSignupAccessToken(a.AccessToken) != (signupPass == 1) {
				continue
			}
			if a.Status == "禁用" {
				continue
			}
			if a.Status == "异常" || a.Status == "封号" {
				continue
			}
			if a.Status == "限流" {
				resetAt := firstNonEmpty(ptrString(a.RateLimitResetAt), ptrString(a.RestoreAt))
				if resetAt != "" {
					rt, err := parseAccountTime(resetAt)
					if err != nil || now.Before(rt) {
						continue
					}
				} else {
					continue
				}
			}
			if !a.ImageQuotaUnknown && a.Quota <= 0 {
				continue
			}
			if needCodex {
				accountType := strings.ToLower(a.Type)
				if strings.ToLower(a.SourceType) != "codex" {
					continue
				}
				if codexPlanType != "" {
					if accountType != codexPlanType {
						continue
					}
				} else if !premium[accountType] {
					continue
				}
			}
			// 检查并发槽
			if p.inflight[a.AccessToken] >= maxConc {
				continue
			}
			p.index = (idx + 1) % len(accounts)
			p.inflight[a.AccessToken]++
			return a.AccessToken, nil
		}
	}
	if needCodex {
		return "", errors.New("no available codex Plus/Team/Pro account")
	}
	return "", errors.New("no available image quota")
}

func (p *accountPool) releaseToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.inflight[token]; v <= 1 {
		delete(p.inflight, token)
	} else {
		p.inflight[token] = v - 1
	}
}

func (p *accountPool) pickTextToken(accounts []Account, planType string) (string, error) {
	return p.pickTextTokenExcluding(accounts, planType, nil)
}

func (p *accountPool) pickTextTokenExcluding(accounts []Account, planType string, excluded map[string]bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	premium := map[string]bool{"plus": true, "pro": true, "team": true}
	for signupPass := 0; signupPass < 2; signupPass++ {
		for offset := 0; offset < len(accounts); offset++ {
			idx := (p.index + offset) % len(accounts)
			a := accounts[idx]
			if a.AccessToken == "" || excluded[a.AccessToken] || a.Status == "禁用" || a.Status == "异常" || a.Status == "封号" || isSignupAccessToken(a.AccessToken) != (signupPass == 1) {
				continue
			}
			lower := strings.ToLower(a.Type)
			if planType == "free" {
				if premium[lower] {
					continue
				}
			} else if planType != "" {
				if lower != planType && (!premium[lower] || lower != planType) {
					continue
				}
			}
			p.index = (idx + 1) % len(accounts)
			return a.AccessToken, nil
		}
	}
	return "", errors.New("no available text account")
}

func (s *Server) pickToken(model, resolution string) (string, error) {
	return s.pickTokenExcluding(model, resolution, nil)
}

func (s *Server) pickTokenExcluding(model, resolution string, excluded map[string]bool) (string, error) {
	account, err := s.pickAccountExcluding(model, resolution, excluded)
	return account.AccessToken, err
}

func (s *Server) pickAccountExcluding(model, resolution string, excluded map[string]bool) (Account, error) {
	accounts := s.store.LoadAccounts()
	needCodex := isCodexImageRequest(model, resolution)
	token, err := s.accountPool.pickTokenExcluding(accounts, needCodex, codexPlanTypeFromModel(model), excluded)
	if err != nil {
		return Account{}, err
	}
	for _, account := range accounts {
		if account.AccessToken == token {
			return account, nil
		}
	}
	return Account{AccessToken: token}, nil
}

func (s *Server) pickTextToken() (string, error) {
	return s.pickTextTokenExcluding(nil, "")
}

func (s *Server) pickTextTokenExcluding(excluded map[string]bool, planType string) (string, error) {
	account, err := s.pickTextAccountExcluding(excluded, planType)
	return account.AccessToken, err
}

func (s *Server) pickTextAccountExcluding(excluded map[string]bool, planType string) (Account, error) {
	accounts := s.store.LoadAccounts()
	token, err := s.accountPool.pickTextTokenExcluding(accounts, planType, excluded)
	if err != nil {
		return Account{}, err
	}
	for _, account := range accounts {
		if account.AccessToken == token {
			return account, nil
		}
	}
	return Account{AccessToken: token}, nil
}
