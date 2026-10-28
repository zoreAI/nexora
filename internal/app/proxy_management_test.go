package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountProxyAssignmentRoutingAndPersistence(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "account", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8001"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "custom", URL: "http://127.0.0.1:8002"}, nil); err != nil {
		t.Fatal(err)
	}
	assignment, err := store.normalizeAccountProxyAssignment("group:" + group.ID)
	if err != nil {
		t.Fatal(err)
	}
	account := Account{AccessToken: "token", Proxy: assignment}
	if err := store.SaveAccounts([]Account{account}); err != nil {
		t.Fatal(err)
	}
	account = store.LoadAccounts()[0]
	s := &Server{store: store}
	for _, test := range []struct{ assignment, want string }{
		{"", "http://127.0.0.1:8002"},
		{"direct", ""},
		{"group:" + group.ID, "http://127.0.0.1:8001"},
		{"http://127.0.0.1:8003", "http://127.0.0.1:8003"},
	} {
		account.Proxy = test.assignment
		got, err := s.selectedProxyForAccount(account)
		if err != nil || got != test.want {
			t.Fatalf("assignment %q selected %q, %v; want %q", test.assignment, got, err, test.want)
		}
	}
	if err := store.DeleteProxyGroup(group.ID); err == nil {
		t.Fatal("deleted a group referenced by an account")
	}
	for _, invalid := range []string{"group:missing", "group:", "ftp://127.0.0.1:21"} {
		if _, err := store.normalizeAccountProxyAssignment(invalid); err == nil {
			t.Fatalf("accepted invalid assignment %q", invalid)
		}
	}
}

func TestAccountProxyUpdateAndImportPreserveAssignment(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "assigned", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8011"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Status: "正常"}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, cfg: Config{AuthKey: "test-key"}}
	update := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/update", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		recorder := httptest.NewRecorder()
		s.handleAccountsUpdate(recorder, req)
		return recorder
	}
	groupAssignment, _ := json.Marshal(map[string]string{"access_token": "token", "proxy": "group:" + group.ID})
	if response := update(string(groupAssignment)); response.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", response.Code, response.Body.String())
	}
	if response := update(`{"access_token":"token","proxy":"group:missing"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d", response.Code)
	}
	account := store.LoadAccounts()[0]
	mergeImportedAccount(&account, accountFromRecord("token", "web", map[string]any{"access_token": "token"}), map[string]any{"access_token": "token"}, false)
	if account.Proxy != "group:"+group.ID {
		t.Fatalf("token-only import removed proxy assignment: %q", account.Proxy)
	}
	if response := update(`{"access_token":"token","proxy":""}`); response.Code != http.StatusOK || store.LoadAccounts()[0].Proxy != "" {
		t.Fatalf("clear assignment failed: status=%d", response.Code)
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:7890", "https://proxy.example:443", "socks5://user:pass@127.0.0.1:1080", "socks5h://[::1]:1080"} {
		if got, err := normalizeProxyURL(raw); err != nil || got == "" {
			t.Errorf("normalizeProxyURL(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"127.0.0.1:7890", "ftp://proxy.example:21", "http://proxy.example:0", "http://proxy.example:65536", "http://proxy.example/path", "http://proxy.example?x=1", "http:///missing-host"} {
		if got, err := normalizeProxyURL(raw); err == nil {
			t.Errorf("normalizeProxyURL(%q) unexpectedly accepted as %q", raw, got)
		}
	}
}

func TestNormalizeProxyURLAcceptsSOCKSNodeFormats(t *testing.T) {
	tests := map[string]string{
		"http://127.0.0.1:7897":                   "http://127.0.0.1:7897",
		"socks://proxy.example:35086:user:pass":   "socks5://user:pass@proxy.example:35086",
		"socks5h://proxy.example:35087:user:pass": "socks5h://user:pass@proxy.example:35087",
		"socks://user:pass@proxy.example:35086":   "socks5://user:pass@proxy.example:35086",
	}
	for raw, want := range tests {
		got, err := normalizeProxyURL(raw)
		if err != nil || got != want {
			t.Errorf("normalizeProxyURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestSaveProxyGroupNormalizesSOCKSNodeFormats(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "mixed", Nodes: []proxyNodeInput{
		{URL: "http://127.0.0.1:7897"},
		{URL: "socks://proxy.example:35086:user:pass"},
		{URL: "socks5h://proxy.example:35087:user:pass"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(group.Nodes) != 3 || group.Nodes[1].URL != "socks5://user:pass@proxy.example:35086" || group.Nodes[2].URL != "socks5h://user:pass@proxy.example:35087" {
		t.Fatal("proxy group did not save canonical SOCKS URLs")
	}
}

func TestDuplicateProxyNodeErrorRedactsCredentials(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	proxyURL := "http://proxy-user:proxy-secret@127.0.0.1:7890"
	_, err := store.SaveProxyGroup(proxyGroupInput{
		Name: "duplicate", Nodes: []proxyNodeInput{{URL: proxyURL}, {URL: proxyURL}},
	})
	if err == nil || !strings.Contains(err.Error(), "重复节点") {
		t.Fatalf("duplicate node error = %v", err)
	}
	if strings.Contains(err.Error(), "proxy-user") || strings.Contains(err.Error(), "proxy-secret") {
		t.Fatalf("proxy credentials appeared in error: %v", err)
	}
}

func TestSQLProxyGroupCreateUpdateDelete(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{
		Name:     "primary",
		Strategy: "round_robin",
		Nodes: []proxyNodeInput{
			{URL: "http://127.0.0.1:7890", ImageConcurrencyLimit: 4},
			{URL: "socks5h://127.0.0.1:1080", Name: "socks", Enabled: boolPtr(true)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.ID == "" || len(group.Nodes) != 2 || group.Nodes[0].Health.State != "unknown" {
		t.Fatalf("unexpected created group: %+v", group)
	}
	firstNodeID := group.Nodes[0].ID
	latency := int64(42)
	if err := store.setProxyNodeHealth(firstNodeID, "healthy", &latency, ""); err != nil {
		t.Fatal(err)
	}
	group, err = store.SaveProxyGroup(proxyGroupInput{ID: group.ID, Name: "primary updated", Strategy: "time_window", Nodes: []proxyNodeInput{{ID: firstNodeID, URL: "http://127.0.0.1:7890"}}})
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "primary updated" || len(group.Nodes) != 1 || group.Nodes[0].Health.State != "healthy" || group.Nodes[0].Health.LatencyMS == nil || *group.Nodes[0].Health.LatencyMS != latency {
		t.Fatalf("update should preserve health for stable node ID: %+v", group)
	}
	if err := store.DeleteProxyGroup(group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProxyGroups(); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProxyGroup(group.ID); err != sql.ErrNoRows {
		t.Fatalf("second delete error = %v, want sql.ErrNoRows", err)
	}
}

func TestProxyNodeURLChangeResetsHealthAndRuntimeCounters(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{
		Name: "changing", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8091"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	node := group.Nodes[0]
	latency := int64(38)
	if err := store.setProxyNodeHealth(node.ID, "unhealthy", &latency, "CONNECT failed"); err != nil {
		t.Fatal(err)
	}
	if err := store.recordProxyNodeRuntimeResult(proxyRoute{NodeID: node.ID, URL: node.URL}, false); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	ref := proxyReference{Mode: "group", GroupID: group.ID}
	if _, err := s.resolveProxyRoute(ref); err == nil {
		t.Fatal("recently unhealthy URL remained selectable")
	}
	updatedURL := "http://127.0.0.1:8092"
	group, err = store.SaveProxyGroup(proxyGroupInput{
		ID: group.ID, Name: group.Name, Nodes: []proxyNodeInput{{ID: node.ID, URL: updatedURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := group.Nodes[0]
	if changed.Health.State != "unknown" || changed.Health.CheckedAt != "" || changed.Health.LatencyMS != nil || changed.Health.Error != "" || changed.SuccessCount != 0 || changed.FailCount != 0 {
		t.Fatalf("new URL inherited old health: %+v", changed)
	}
	route, err := s.resolveProxyRoute(ref)
	if err != nil || route.URL != updatedURL {
		t.Fatalf("new URL route = %+v, %v", route, err)
	}
	if err := store.recordProxyNodeRuntimeResult(proxyRoute{NodeID: node.ID, URL: node.URL}, false); err != nil {
		t.Fatal(err)
	}
	group, err = store.SaveProxyGroup(proxyGroupInput{
		ID: group.ID, Name: group.Name, Nodes: []proxyNodeInput{{ID: node.ID, URL: updatedURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.Nodes[0].Health.State != "unknown" || group.Nodes[0].FailCount != 0 {
		t.Fatalf("old URL result changed new URL health: %+v", group.Nodes[0])
	}
	if err := store.setProxyNodeHealth(node.ID, "healthy", &latency, ""); err != nil {
		t.Fatal(err)
	}
	group, err = store.SaveProxyGroup(proxyGroupInput{
		ID: group.ID, Name: group.Name, Nodes: []proxyNodeInput{{ID: node.ID, URL: updatedURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.Nodes[0].Health.State != "healthy" || group.Nodes[0].Health.LatencyMS == nil || *group.Nodes[0].Health.LatencyMS != latency {
		t.Fatalf("stable URL lost its health: %+v", group.Nodes[0])
	}
}

func TestConfiguredProxyUsesSQLDefaultGroup(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{
		Name: "rotating", Strategy: "round_robin",
		Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7901"}, {URL: "http://127.0.0.1:7902"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: group.ID}, nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, cfg: Config{Proxy: "http://127.0.0.1:7999"}}
	first, err := s.selectedProxy()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.selectedProxy()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || (first != "http://127.0.0.1:7901" && first != "http://127.0.0.1:7902") || (second != "http://127.0.0.1:7901" && second != "http://127.0.0.1:7902") {
		t.Fatalf("round robin proxies = %q, %q", first, second)
	}
}

func TestProxyGroupSkipsRecentlyUnhealthyNodes(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{
		Name: "mixed", Strategy: "round_robin",
		Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8021"}, {URL: "http://127.0.0.1:8022"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setProxyNodeHealth(group.Nodes[0].ID, "unhealthy", nil, "CONNECT failed"); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	for range 3 {
		got, err := s.resolveProxyReference(proxyReference{Mode: "group", GroupID: group.ID})
		if err != nil || got != group.Nodes[1].URL {
			t.Fatalf("selected %q, %v; want healthy node", got, err)
		}
	}
	stale := time.Now().Add(-proxyUnhealthyCooldown - time.Second).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.Exec("UPDATE app_proxy_nodes SET checked_at = "+store.bind(1)+" WHERE id = "+store.bind(2), stale, group.Nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].Nodes[0].Health.State != "unhealthy" {
		t.Fatal("historical node failure was discarded")
	}
	seenRecovered := false
	for range 3 {
		got, err := s.resolveProxyReference(proxyReference{Mode: "group", GroupID: group.ID})
		if err != nil {
			t.Fatal(err)
		}
		seenRecovered = seenRecovered || got == group.Nodes[0].URL
	}
	if !seenRecovered {
		t.Fatal("stale unhealthy node was never retried")
	}
}

func TestProxyGroupAllUnhealthyUsesFallbackButAccountBindingFailsClosed(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	primary, err := store.SaveProxyGroup(proxyGroupInput{Name: "primary", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8031"}}})
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := store.SaveProxyGroup(proxyGroupInput{Name: "fallback", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8032"}}})
	if err != nil {
		t.Fatal(err)
	}
	fallbackRef := proxyReference{Mode: "group", GroupID: fallback.ID}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: primary.ID}, &fallbackRef); err != nil {
		t.Fatal(err)
	}
	if err := store.setProxyNodeHealth(primary.Nodes[0].ID, "unhealthy", nil, "CONNECT failed"); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-proxyUnhealthyCooldown - time.Second).UTC().Format(time.RFC3339Nano)
	staleNode := primary.Nodes[0]
	staleNode.Health = ProxyHealth{State: "unhealthy", CheckedAt: stale}
	if got := aggregateProxyHealth([]ProxyNode{staleNode}).State; got != "unknown" {
		t.Fatalf("expired failure group health = %q, want unknown", got)
	}
	if got := aggregateProxyHealth([]ProxyNode{{Enabled: false}}).State; got != "unknown" {
		t.Fatalf("disabled-only group health = %q, want unknown", got)
	}
	s := &Server{store: store}
	if got, err := s.selectedProxy(); err != nil || got != fallback.Nodes[0].URL {
		t.Fatalf("fallback selected %q, %v", got, err)
	}
	if got, err := s.selectedProxyForAccount(Account{Proxy: "group:" + primary.ID}); err == nil || got != "" {
		t.Fatalf("explicit account proxy unexpectedly fell back: %q, %v", got, err)
	}
}

func TestTimeWindowProxySelectionIsStableWithinWindow(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{
		Name: "window", Strategy: "time_window", RotationIntervalMinutes: 60,
		Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7911"}, {URL: "http://127.0.0.1:7912"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: group.ID}, nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	got, err := s.selectedProxy()
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.selectedProxy()
	if err != nil {
		t.Fatal(err)
	}
	if got != again {
		t.Fatalf("time-window selection changed within the same window: %q, %q", got, again)
	}
}

func TestDeleteProxyGroupRejectsActiveDefault(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "active", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7921"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: group.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProxyGroup(group.ID); err == nil {
		t.Fatal("deleted a group still in use")
	}
	groups, err := store.LoadProxyGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("active group was removed: groups=%d err=%v", len(groups), err)
	}
}

func TestProxyGroupWithoutEnabledNodesFailsClosed(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	disabled := false
	primary, err := store.SaveProxyGroup(proxyGroupInput{Name: "primary", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7931", Enabled: &disabled}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: primary.ID}, nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, cfg: Config{Proxy: "http://127.0.0.1:7999"}}
	if got, err := s.selectedProxy(); err == nil {
		t.Fatalf("unavailable group unexpectedly selected %q", got)
	}
	if client, err := s.newUpstreamClientForAccount(Account{}); err == nil || client != nil {
		t.Fatalf("unavailable group created client: client=%v err=%v", client, err)
	}
	fallback, err := store.SaveProxyGroup(proxyGroupInput{Name: "fallback", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7932"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: primary.ID}, &proxyReference{Mode: "group", GroupID: fallback.ID}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.selectedProxy(); err != nil || got != "http://127.0.0.1:7932" {
		t.Fatalf("fallback selection = %q, %v", got, err)
	}
}

func TestProxyDefaultsRejectUnknownGroup(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: "missing"}, nil); err == nil {
		t.Fatal("accepted missing default group")
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "direct"}, &proxyReference{Mode: "group", GroupID: "missing"}); err == nil {
		t.Fatal("accepted missing fallback group")
	}
}

func TestAccountProxyStorageRejectsMissingGroup(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	if err := store.SaveAccounts([]Account{{AccessToken: "token", Proxy: "group:missing"}}); err == nil {
		t.Fatal("stored account with missing proxy group")
	}
	if accounts := store.LoadAccounts(); len(accounts) != 0 {
		t.Fatalf("invalid account proxy persisted: %+v", accounts)
	}
}

func TestConcurrentProxyGroupReferenceAndDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := NewStore(dir)
	defer store.Close()
	other := NewStore(dir)
	defer other.Close()
	for _, owner := range []string{"default", "account-group", "account"} {
		t.Run(owner, func(t *testing.T) {
			group, err := store.SaveProxyGroup(proxyGroupInput{Name: owner, Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:8191"}}})
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			saved := make(chan error, 1)
			deleted := make(chan error, 1)
			go func() {
				<-start
				switch owner {
				case "default":
					saved <- store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: group.ID}, nil)
				case "account-group":
					_, err := store.SaveAccountGroup(accountGroupInput{Name: owner, Proxy: "group:" + group.ID})
					saved <- err
				default:
					saved <- store.SaveAccounts([]Account{{AccessToken: "token", Proxy: "group:" + group.ID}})
				}
			}()
			go func() {
				<-start
				deleted <- other.DeleteProxyGroup(group.ID)
			}()
			close(start)
			saveErr, deleteErr := <-saved, <-deleted
			if saveErr == nil && deleteErr == nil {
				t.Fatal("reference and proxy group deletion both succeeded")
			}
			groups, err := store.LoadProxyGroups()
			if err != nil {
				t.Fatal(err)
			}
			present := false
			for _, item := range groups {
				present = present || item.ID == group.ID
			}
			if saveErr == nil && !present {
				t.Fatal("saved reference points to deleted proxy group")
			}
			if deleteErr == nil && present {
				t.Fatal("proxy group remained after successful deletion")
			}
			if owner == "default" {
				if err := store.SaveProxyDefaults(proxyReference{Mode: "direct"}, nil); err != nil {
					t.Fatal(err)
				}
			} else if owner == "account-group" {
				for _, item := range mustLoadAccountGroups(t, store) {
					if item.Proxy == "group:"+group.ID {
						if err := store.DeleteAccountGroup(item.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
			} else if err := store.SaveAccounts(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mustLoadAccountGroups(t *testing.T, store *Store) []AccountGroup {
	t.Helper()
	groups, err := store.LoadAccountGroups()
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

func TestProxyDefaultsRejectSameFallbackGroup(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "only", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7951"}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := proxyReference{Mode: "group", GroupID: group.ID}
	if err := store.SaveProxyDefaults(ref, &ref); err == nil {
		t.Fatal("accepted identical default and fallback groups")
	}
}

func TestProxyNodeCannotMoveBetweenGroups(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	defer store.Close()
	first, err := store.SaveProxyGroup(proxyGroupInput{Name: "first", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7941"}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveProxyGroup(proxyGroupInput{Name: "second", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7942"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveProxyGroup(proxyGroupInput{ID: second.ID, Name: "second", Nodes: []proxyNodeInput{{ID: first.Nodes[0].ID, URL: "http://127.0.0.1:7943"}}})
	if err == nil {
		t.Fatal("node was moved from another group")
	}
	groups, err := store.LoadProxyGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group.ID == first.ID && (len(group.Nodes) != 1 || group.Nodes[0].URL != "http://127.0.0.1:7941") {
			t.Fatalf("first group changed: %+v", group)
		}
	}
}

func TestSettingsSaveKeepsSelectedProxyGroupWhenURLUnchanged(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "data"))
	defer store.Close()
	group, err := store.SaveProxyGroup(proxyGroupInput{Name: "primary", Nodes: []proxyNodeInput{{URL: "http://127.0.0.1:7961"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProxyDefaults(proxyReference{Mode: "group", GroupID: group.ID}, nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{root: root, store: store, cfg: Config{AuthKey: "test-key", Proxy: "http://127.0.0.1:7960", Extra: map[string]any{}}}
	request := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		response := httptest.NewRecorder()
		s.handleSettings(response, req)
		return response.Code
	}
	if status := request(`{"proxy":"http://127.0.0.1:7960","refresh_account_interval_minute":30}`); status != http.StatusOK {
		t.Fatalf("unchanged settings status=%d", status)
	}
	if got, err := s.selectedProxy(); err != nil || got != "http://127.0.0.1:7961" {
		t.Fatalf("unchanged URL replaced group: %q, %v", got, err)
	}
	if status := request(`{"proxy":"http://127.0.0.1:7962"}`); status != http.StatusOK {
		t.Fatalf("changed settings status=%d", status)
	}
	if got, err := s.selectedProxy(); err != nil || got != "http://127.0.0.1:7962" {
		t.Fatalf("changed URL did not switch to custom: %q, %v", got, err)
	}
}

func boolPtr(value bool) *bool { return &value }
