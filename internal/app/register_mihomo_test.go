package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMihomoRegistrationReadsAndBindsAnyGroupWithoutGlobalSwitch(t *testing.T) {
	var mu sync.Mutex
	var listeners []net.Listener
	selected := "node-b"
	reloads := 0
	proxies := map[string]mihomoProxy{
		"arbitrary group": {Type: "Selector", Now: selected, All: []string{"nested", "DIRECT", "node-a", "node-b", "node-a"}},
		"nested":          {Type: "Selector", Now: "node-a", All: []string{"node-a"}},
		"node-a":          {Type: "Trojan"}, "node-b": {Type: "Vless"}, "DIRECT": {Type: "Direct"},
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/proxies":
			p := proxies["arbitrary group"]
			p.Now = selected
			proxies["arbitrary group"] = p
			json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
		case r.Method == "GET" && r.URL.Path == "/configs":
			json.NewEncoder(w).Encode(map[string]any{"mode": "rule", "mixed-port": 7897})
		case r.Method == "PUT" && r.URL.Path == "/configs":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			var raw map[string]any
			if yaml.Unmarshal([]byte(body["payload"]), &raw) != nil {
				w.WriteHeader(400)
				return
			}
			for _, l := range listeners {
				l.Close()
			}
			listeners = nil
			for _, value := range raw["listeners"].([]any) {
				l := value.(map[string]any)
				if !strings.HasPrefix(strAny(l["name"], ""), "nexora-register-") {
					continue
				}
				listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmtInt(intAny(l["port"], 0))))
				if err != nil {
					w.WriteHeader(400)
					return
				}
				listeners = append(listeners, listener)
			}
			if strAny(raw["mode"], "") != "rule" || intAny(raw["mixed-port"], 0) != 7897 {
				t.Error("live settings were lost")
			}
			selected = "node-a"
			reloads++
			w.WriteHeader(204)
		case r.Method == "PUT" && r.URL.Path == "/proxies/arbitrary group":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			selected = body["name"]
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer core.Close()
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	root := t.TempDir()
	configPath := filepath.Join(root, "clash.yaml")
	original := "mode: global\nmixed-port: 7897\nsecret: test-secret\nlisteners: []\n"
	if err := os.WriteFile(configPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	defer store.Close()
	defaultRef := proxyReference{Mode: "custom", URL: "http://127.0.0.1:7897"}
	if err := store.SaveProxyDefaults(defaultRef, nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{AuthKey: "admin"}, store: store, dataDir: root}
	s.register = newRegisterService(root, store)
	s.register.server = s
	// Reserve then release two adjacent ports that this isolated fake core owns.
	portStart := 0
	for port := 23001; port < 24000; port += 2 {
		a, e1 := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmtInt(port)))
		if e1 != nil {
			continue
		}
		b, e2 := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmtInt(port+1)))
		a.Close()
		if e2 == nil {
			b.Close()
			portStart = port
			break
		}
	}
	if portStart == 0 {
		t.Fatal("no free test ports")
	}
	opts := registerMihomoSettings{Controller: core.URL, ConfigPath: configPath, Group: "arbitrary group", PortStart: portStart}
	call := func(path string, admin bool) *httptest.ResponseRecorder {
		data, _ := json.Marshal(opts)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(data)))
		if admin {
			req.Header.Set("Authorization", "Bearer admin")
		}
		r := httptest.NewRecorder()
		s.handleRegisterMihomo(r, req)
		return r
	}
	if r := call("/api/register/mihomo/groups", false); r.Code != 401 {
		t.Fatalf("anonymous=%d", r.Code)
	}
	if r := call("/api/register/mihomo/groups", true); r.Code != 200 || strings.Contains(r.Body.String(), "test-secret") {
		t.Fatalf("discover=%s", r.Body.String())
	}
	if reloads != 0 {
		t.Fatal("reading groups changed the core")
	}
	for range 2 {
		r := call("/api/register/mihomo/apply", true)
		if r.Code != 200 {
			t.Fatalf("apply %d: %s", r.Code, r.Body.String())
		}
	}
	groups, _ := store.LoadProxyGroups()
	if len(groups) != 1 || len(groups[0].Nodes) != 2 || groups[0].Strategy != "round_robin" {
		t.Fatalf("groups=%+v", groups)
	}
	if selected != "node-b" {
		t.Fatal("the user's active selection changed")
	}
	defaults, _ := store.LoadProxyDefaults()
	if defaults.Default != defaultRef {
		t.Fatal("default proxy changed")
	}
	if s.register.snapshot().Proxy != "group:"+groups[0].ID || s.register.snapshot().Enabled {
		t.Fatal("registration routing/start state is wrong")
	}
	data, _ := os.ReadFile(configPath)
	if string(data) != original {
		t.Fatal("source Clash file modified")
	}
	// A new registration run recreates listeners after a core restart.
	mu.Lock()
	for _, l := range listeners {
		l.Close()
	}
	listeners = nil
	mu.Unlock()
	s.register.mu.Lock()
	s.register.cfg.Enabled = true
	s.register.mu.Unlock()
	probed := false
	s.register.passwordStepProbe = func(context.Context, RegisterConfig) error {
		probed = true
		if reloads != 3 {
			t.Fatal("registration probe ran before restoring Mihomo listeners")
		}
		return errors.New("stop test before claiming a mailbox")
	}
	s.register.runRegisterJob(context.Background())
	if !probed {
		t.Fatal("registration did not reach its probe after restoring listeners")
	}
	if reloads != 3 {
		t.Fatalf("reloads=%d", reloads)
	}
	// Repair reuses live listeners, and restores them only after core restart.
	if err := s.ensureAccountRepairMihomo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reloads != 3 {
		t.Fatal("repair unnecessarily reloaded active Mihomo listeners")
	}
	mu.Lock()
	for _, l := range listeners {
		l.Close()
	}
	listeners = nil
	mu.Unlock()
	s.register.mu.Lock()
	s.register.cfg.Enabled = true
	s.register.cfg.Stats.JobKind = "repair_abnormal"
	s.register.mu.Unlock()
	if err := s.ensureAccountRepairMihomo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reloads != 4 {
		t.Fatal("repair did not restore listeners after core restart")
	}
	s.register.mu.Lock()
	s.register.cfg.Stats.Running = 1
	s.register.mu.Unlock()
	if r := call("/api/register/mihomo/apply", true); r.Code != 409 {
		t.Fatalf("active worker apply=%d", r.Code)
	}
}

func TestMihomoRegistrationPlanPreservesUnrelatedListeners(t *testing.T) {
	raw := map[string]any{"mixed-port": 7897, "listeners": []any{map[string]any{"name": "existing", "port": 19000, "proxy": "something"}}}
	updated, nodes, err := mihomoListenerPlan(raw, []string{"a", "b"}, 18001)
	if err != nil || len(nodes) != 2 {
		t.Fatal(err)
	}
	if len(raw["listeners"].([]any)) != 1 || len(updated["listeners"].([]any)) != 3 {
		t.Fatal("original/other listeners changed")
	}
	if _, _, err := mihomoListenerPlan(raw, []string{"a"}, 19000); err == nil {
		t.Fatal("port conflict accepted")
	}
	if _, _, err := mihomoListenerPlan(raw, []string{"a", "b"}, 65535); err == nil {
		t.Fatal("port overflow accepted")
	}
	if _, err := mihomoConcreteMembers(map[string]mihomoProxy{"empty": {Type: "Selector", All: []string{}}}, "empty"); err == nil {
		t.Fatal("empty group accepted")
	}
}

func TestMihomoRegistrationStopsBeforeProbeWhenCoreUnavailable(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	defer store.Close()
	s := &Server{store: store, dataDir: root}
	s.register = newRegisterService(root, store)
	s.register.server = s
	s.register.cfg.Enabled = true
	s.register.cfg.Proxy = "group:managed"
	if err := store.SaveDocument("register-mihomo.json", registerMihomoSettings{
		ProxyGroupID: "managed", ConfigPath: filepath.Join(root, "missing.yaml"),
	}); err != nil {
		t.Fatal(err)
	}
	s.register.passwordStepProbe = func(context.Context, RegisterConfig) error {
		t.Fatal("probe must not run with unavailable registration routes")
		return nil
	}
	s.register.runRegisterJob(context.Background())
	cfg := s.register.snapshot()
	if cfg.Enabled || cfg.Stats.Done != 0 || len(cfg.Logs) < 2 || !strings.Contains(cfg.Logs[0].Text, "准备 Mihomo 注册代理失败") {
		t.Fatalf("unexpected stopped job: %+v", cfg)
	}
}

func fmtInt(value int) string { return strconv.Itoa(value) }
