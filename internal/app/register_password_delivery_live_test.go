package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in only. Keep all intermediate credentials in a private store so a
// successful password change is recoverable even if a subsequent step fails.
func TestLivePasswordDelivery(t *testing.T) {
	email := os.Getenv("NEXORA_LIVE_PASSWORD_EMAIL")
	if email == "" {
		t.Skip("set NEXORA_LIVE_PASSWORD_EMAIL for one authorized account")
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
		client := &http.Client{Timeout: 20 * time.Second}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal("read running app failed")
		}
		defer r.Body.Close()
		if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(out) != nil {
			t.Fatal("read running app response failed")
		}
	}
	var state struct {
		Register RegisterConfig `json:"register"`
	}
	get("/api/register", &state)
	if state.Register.Enabled {
		t.Fatal("registration or repair submission is active")
	}
	if state.Register.Stats.Running > 0 {
		// A drained queue can still be finishing other accounts. Only test this
		// account if its last log explicitly says it already completed; never
		// race the same email across the running server and this private store.
		completed := false
		for _, item := range state.Register.Logs {
			if strings.Contains(item.Text, email+" ") {
				completed = strings.Contains(item.Text, "异常账号修复失败:") || strings.Contains(item.Text, "异常账号修复成功")
			}
		}
		if !completed {
			t.Fatal("selected account may still be active; wait for its completion")
		}
	}
	var accounts struct {
		Items []Account `json:"items"`
	}
	get("/api/accounts", &accounts)
	var account Account
	for _, a := range accounts.Items {
		if strings.EqualFold(ptrString(a.Email), email) {
			account = a
			break
		}
	}
	if account.AccessToken == "" || ptrString(account.Password) == "" || account.Status != "异常" || account.PasswordStatus == "confirmed" || ptrString(account.MFASecret) != "" {
		t.Fatal("choose one abnormal account with an unconfirmed candidate and no MFA")
	}
	proxyURL := state.Register.Proxy
	if strings.HasPrefix(proxyURL, "group:") {
		var groups struct {
			Groups []ProxyGroup `json:"groups"`
		}
		get("/api/proxy/groups", &groups)
		id := strings.TrimPrefix(proxyURL, "group:")
		proxyURL = ""
		for _, g := range groups.Groups {
			if g.ID == id && g.Enabled {
				for _, n := range g.Nodes {
					if n.Enabled {
						proxyURL = n.URL
						break
					}
				}
			}
		}
		if proxyURL == "" {
			t.Fatal("no enabled registration proxy")
		}
	}
	dir, err := os.MkdirTemp("", "nexora-password-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("private result store:", dir)
	cfg.Proxy = proxyURL
	s := &Server{root: root, cfg: cfg, store: NewStore(dir), accountPool: newAccountPool(&cfg)}
	defer s.store.Close()
	if err := s.store.SaveAccounts([]Account{account}); err != nil {
		t.Fatal("save initial account failed")
	}
	s.register = newRegisterService(dir, s.store)
	s.register.server = s
	s.register.cfg = state.Register
	s.register.cfg.Proxy = proxyURL
	s.register.cfg.Enabled = false
	provider, err := s.register.createProviderWithProxy(s.register.cfg.Mail, proxyURL)
	if err != nil {
		t.Fatal("create original provider failed")
	}
	defer provider.Close()
	session := provider.Session().(*registerSession)
	session.proxyRoute = proxyRoute{URL: proxyURL}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, accountRepairProgressKey{}, func(stage string) { t.Log(stage) })
	mailbox := &registerMailbox{Address: email, Provider: "icloud_api"}
	recovered, err := s.register.confirmRecoveredPassword(ctx, session, account, func(ctx context.Context, cutoff time.Time) (string, error) {
		mailbox.CodeNotBefore = cutoff
		return provider.WaitForCode(ctx, mailbox)
	})
	if err == nil {
		err = s.register.enableRegisterMFAWithFailover(ctx, session, recovered.AccessToken, uuid4())
	}
	result := s.store.LoadAccounts()
	if saveErr := writePrivateJSON(filepath.Join(dir, "result-accounts.json"), result); saveErr != nil {
		t.Fatal("save private result failed")
	}
	if err != nil {
		t.Fatal(strings.ReplaceAll(err.Error(), ptrString(account.Password), "[redacted]"))
	}
	if len(result) != 1 || result[0].PasswordStatus != "confirmed" || result[0].MFAStatus != "enabled" {
		t.Fatal("password or MFA incomplete")
	}
	t.Log("password confirmed and MFA saved for", email)
}
