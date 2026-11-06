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

// Explicit opt-in only: resumes one existing mailbox against the real service.
// Credentials and any resulting account are retained in a private temporary
// store; the running application's database is not written by this test.
func TestLivePendingWebSignup(t *testing.T) {
	email := os.Getenv("NEXORA_LIVE_PENDING_EMAIL")
	if email == "" {
		t.Skip("set NEXORA_LIVE_PENDING_EMAIL to test one real pending registration")
	}
	root, _ := filepath.Abs("../..")
	cfg, err := loadConfig(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal("load app config failed")
	}
	get := func(path string, out any) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:3000"+path, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.AuthKey)
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("read running app failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(out) != nil {
			t.Fatal("read running app response failed")
		}
	}
	var register struct {
		Register RegisterConfig `json:"register"`
	}
	get("/api/register", &register)
	if register.Register.Enabled || register.Register.Stats.Running > 0 {
		t.Fatal("registration is active; do not test concurrently")
	}
	var pending registerPendingPassword
	for _, item := range register.Register.PendingPasswords {
		if strings.EqualFold(item.Email, email) {
			pending = item
			break
		}
	}
	if pending.Password == "" {
		t.Fatal("selected pending password is missing")
	}
	var accounts struct {
		Items []Account `json:"items"`
	}
	get("/api/accounts", &accounts)
	for _, account := range accounts.Items {
		if strings.EqualFold(ptrString(account.Email), email) {
			t.Fatal("selected email is already pooled; choose a tokenless registration")
		}
	}
	proxyURL := register.Register.Proxy
	if strings.HasPrefix(proxyURL, "group:") {
		var groups struct {
			Groups []ProxyGroup `json:"groups"`
		}
		get("/api/proxy/groups", &groups)
		id := strings.TrimPrefix(proxyURL, "group:")
		proxyURL = ""
		for _, group := range groups.Groups {
			if group.ID == id && group.Enabled {
				for _, node := range group.Nodes {
					if node.Enabled {
						proxyURL = node.URL
						break
					}
				}
			}
		}
		if proxyURL == "" {
			t.Fatal("configured registration proxy group has no enabled nodes")
		}
	}
	dir, err := os.MkdirTemp("", "nexora-web-signup-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("private result store:", dir)
	cfg.Proxy = proxyURL
	s := &Server{root: root, cfg: cfg, store: NewStore(dir), accountPool: newAccountPool(&cfg)}
	defer s.store.Close()
	s.register = newRegisterService(dir, s.store)
	s.register.server = s
	s.register.cfg = register.Register
	s.register.cfg.Enabled = false
	s.register.cfg.Proxy = proxyURL
	s.register.cfg.PendingPasswords = []registerPendingPassword{pending}
	s.register.cfg.RecoveryEmails = nil
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, accountRepairProgressKey{}, func(stage string) { t.Log(stage) })
	provider, err := s.register.createProviderWithProxy(s.register.cfg.Mail, proxyURL)
	if err != nil {
		t.Fatal("create original mailbox provider failed")
	}
	defer provider.Close()
	session := provider.Session().(*registerSession)
	session.proxyRoute = proxyRoute{URL: proxyURL}
	mailbox := pending.Mailbox
	if mailbox == nil {
		mailbox = &registerMailbox{Address: email, Provider: "icloud_api"}
	}
	profile := registerProfileConfig{Name: s.register.cfg.ProfileName, Birthdate: s.register.cfg.ProfileBirthdate}
	var tokens map[string]any
	continuation := os.Getenv("NEXORA_LIVE_PENDING_CONTINUATION") == "1"
	if continuation {
		// Exercise the same coordinator as the repair button, including password
		// confirmation and MFA, while keeping results in this private store.
		err = s.register.resumePendingRegisterAccount(ctx, provider, mailbox, pending.Password, 1)
	} else if resultPath := os.Getenv("NEXORA_LIVE_RESULT_PATH"); resultPath != "" {
		data, readErr := os.ReadFile(resultPath)
		var records []Account
		if readErr != nil || json.Unmarshal(data, &records) != nil || len(records) != 1 || !strings.EqualFold(ptrString(records[0].Email), email) || records[0].AccessToken == "" {
			t.Fatal("previous private result does not contain this account")
		}
		tokens = map[string]any{"access_token": records[0].AccessToken, "email": email, "password_confirmed": records[0].PasswordStatus == "confirmed"}
	} else {
		tokens, err = registerPendingInBrowser(ctx, session, email, pending.Password, profile, func(ctx context.Context, cutoff time.Time) (string, error) {
			mailbox.CodeNotBefore = cutoff
			return provider.WaitForCode(ctx, mailbox)
		})
	}
	if err == nil && !continuation {
		session.passwordConfirmed = boolAny(tokens["password_confirmed"], false)
		err = s.register.completeRecoveredRegisterAccount(ctx, provider, mailbox, pending.Password, tokens, 1)
	}
	result := s.store.LoadAccounts()
	if saveErr := writePrivateJSON(filepath.Join(dir, "result-accounts.json"), result); saveErr != nil {
		t.Fatal("save result accounts failed")
	}
	if err != nil {
		message := strings.ReplaceAll(err.Error(), pending.Password, "[redacted]")
		t.Fatal(message)
	}
	if len(result) != 1 || result[0].PasswordStatus != "confirmed" || result[0].MFAStatus != "enabled" || result[0].Status != "正常" {
		t.Fatal("account/password/MFA recovery is incomplete; inspect private result store")
	}
	t.Log("real recovery, confirmed password and saved MFA completed for", email)
}
