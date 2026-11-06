package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicitly selected, authorized account only. Persist every intermediate
// credential to a private store even when a later recovery stage fails.
func TestLiveAccountRecovery(t *testing.T) {
	email := os.Getenv("NEXORA_LIVE_REPAIR_EMAIL")
	if email == "" {
		t.Skip("set NEXORA_LIVE_REPAIR_EMAIL for one authorized account")
	}
	root, _ := filepath.Abs("../..")
	cfg, err := loadConfig(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal("load config failed")
	}
	get := func(path string, out any) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:3000"+path, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.AuthKey)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("read running app failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(out) != nil {
			t.Fatal("read running app response failed")
		}
	}
	var state struct {
		Register RegisterConfig `json:"register"`
	}
	get("/api/register", &state)
	if state.Register.Enabled || state.Register.Stats.Running > 0 {
		t.Fatal("registration is active")
	}
	if id := os.Getenv("NEXORA_LIVE_REPAIR_JOB_ID"); id != "" {
		var progress accountRefreshProgress
		get("/api/accounts/repair/progress/"+id, &progress)
		finished := progress.Done
		for _, failure := range progress.Errors {
			if strings.EqualFold(failure.Account, email) {
				finished = true
			}
		}
		if !finished {
			t.Fatal("selected account might still be running")
		}
	}
	var records struct {
		Items []Account `json:"items"`
	}
	get("/api/accounts", &records)
	var account Account
	for _, a := range records.Items {
		if strings.EqualFold(ptrString(a.Email), email) {
			account = a
			break
		}
	}
	if account.Status != "异常" || account.AccessToken == "" {
		t.Fatal("select an existing abnormal account")
	}
	dir, err := os.MkdirTemp("", "nexora-account-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("private result store:", dir)
	s := &Server{root: root, cfg: cfg, store: NewStore(dir), accountPool: newAccountPool(&cfg)}
	defer s.store.Close()
	if err := s.store.SaveAccounts([]Account{account}); err != nil {
		t.Fatal("save initial account failed")
	}
	s.register = newRegisterService(dir, s.store)
	s.register.server = s
	s.register.cfg = state.Register
	s.register.cfg.Enabled = false
	if strings.HasPrefix(state.Register.Proxy, "group:") {
		var groups struct {
			Groups []ProxyGroup `json:"groups"`
		}
		get("/api/proxy/groups", &groups)
		for _, g := range groups.Groups {
			if g.ID != strings.TrimPrefix(state.Register.Proxy, "group:") || !g.Enabled {
				continue
			}
			for _, n := range g.Nodes {
				if n.Enabled {
					s.register.cfg.Proxy = n.URL
					break
				}
			}
		}
		if strings.HasPrefix(s.register.cfg.Proxy, "group:") {
			t.Fatal("no enabled proxy")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountRepairTimeout(state.Register.Mail))
	defer cancel()
	ctx = context.WithValue(ctx, accountRepairProgressKey{}, func(stage string) { t.Log(stage) })
	if os.Getenv("NEXORA_LIVE_REPAIR_VERIFY_PASSWORD") == "1" {
		var next Account
		next, err = s.reLoginAccountWithPasswordSecret(ctx, account, ptrString(account.MFASecret))
		if err == nil && next.PasswordStatus != "confirmed" {
			err = errors.New("mail login did not verify the saved password")
		}
		if err == nil {
			err = s.persistReLoginAccount(account.AccessToken, next)
		}
	} else if os.Getenv("NEXORA_LIVE_REPAIR_RESET_ONLY") == "1" {
		route, routeErr := s.register.accountRepairProxyRoute(ctx)
		if routeErr != nil {
			t.Fatal("resolve proxy failed")
		}
		session, sessionErr := s.newAccountLoginSession(route.URL, 60)
		if sessionErr != nil {
			t.Fatal("create session failed")
		}
		defer session.close()
		session.proxyRoute = route
		session.loginMFASecret = ptrString(account.MFASecret)
		session.onPasswordAccepted = func() error { _, err := s.persistConfirmedPassword(account, account); return err }
		mailbox := &registerMailbox{Address: email, Provider: "icloud_api"}
		var tokens map[string]any
		waitCode := func(ctx context.Context, cutoff time.Time) (string, error) {
			return s.waitAccountEmailCode(ctx, session, account, email, ptrString(account.EmailPassword), cutoff)
		}
		tokens, err = setRegisterPasswordInBrowser(ctx, session, mailbox, ptrString(account.Password), route.URL, false, waitCode)
		if err == nil {
			if !strings.EqualFold(strAny(tokens["email"], ""), email) || strAny(tokens["access_token"], "") == "" {
				t.Fatal("reset returned wrong account")
			}
			next := account
			next.AccessToken = strAny(tokens["access_token"], "")
			next.RefreshToken = nil
			next.IDToken = nil
			next, err = s.persistConfirmedPassword(account, next)
			if err == nil {
				err = s.completeRecoveredAccount(ctx, next)
			}
		}
	} else {
		err = s.repairAccountTarget(ctx, accountRepairTarget{account: account, email: email}, 1)
	}
	result := s.store.LoadAccounts()
	if writePrivateJSON(filepath.Join(dir, "result-accounts.json"), result) != nil {
		t.Fatal("save private result failed")
	}
	if err != nil {
		message := err.Error()
		for _, secret := range []string{account.AccessToken, ptrString(account.Password), ptrString(account.MFASecret), ptrString(account.RefreshToken), ptrString(account.IDToken), account.Proxy} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		t.Fatal(message)
	}
	if len(result) != 1 || result[0].Status != "正常" {
		t.Fatal("recovery did not finish")
	}
	t.Log("account recovery completed for", email)
}
