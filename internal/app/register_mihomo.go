package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// This connector reuses an existing core. It never starts Mihomo or changes
// the system proxy/default Nexora route. Dedicated listeners pin each task.
type registerMihomoSettings struct {
	Controller   string `json:"controller"`
	Secret       string `json:"secret,omitempty"`
	ConfigPath   string `json:"config_path"`
	Group        string `json:"group"`
	PortStart    int    `json:"port_start"`
	ProxyGroupID string `json:"proxy_group_id,omitempty"`
}

type mihomoProxy struct {
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}

type mihomoConnection struct {
	settings registerMihomoSettings
	client   *http.Client
	base     string
	raw      map[string]any
}

func openRegisterMihomo(opts registerMihomoSettings) (*mihomoConnection, error) {
	if opts.ConfigPath == "" {
		userDir, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opts.ConfigPath = filepath.Join(userDir, "Library", "Application Support", "io.github.clash-verge-rev.clash-verge-rev", "clash-verge.yaml")
	}
	data, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return nil, errors.New("无法读取 Mihomo 运行配置，请指定 Nexora 可读取的配置文件路径")
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("Mihomo 运行配置 YAML 无效")
	}
	if opts.Controller == "" {
		if socket := strAny(raw["external-controller-unix"], ""); socket != "" {
			opts.Controller = "unix://" + socket
		} else if addr := strAny(raw["external-controller"], ""); addr != "" {
			opts.Controller = "http://" + addr
		}
	}
	if opts.Secret == "" {
		opts.Secret = strAny(raw["secret"], "")
	}
	if opts.PortStart == 0 {
		opts.PortStart = 18001
	}
	if opts.PortStart < 1024 || opts.PortStart > 65535 {
		return nil, errors.New("起始端口须在 1024 到 65535 之间")
	}
	transport := &http.Transport{} // Never use HTTP_PROXY to reach the controller.
	base := strings.TrimRight(opts.Controller, "/")
	if strings.HasPrefix(base, "unix://") {
		socket := strings.TrimPrefix(opts.Controller, "unix://")
		if !filepath.IsAbs(socket) {
			return nil, errors.New("Mihomo Unix socket 必须是绝对路径")
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}
		base = "http://mihomo"
	} else {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("请填写 Mihomo 控制 API 的 HTTP(S) 地址或 unix:///socket/path；代理端口 7897 不能用作控制 API")
		}
		ip := net.ParseIP(u.Hostname())
		if !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("注册专用出口目前要求 Mihomo 与 Nexora 同机运行，请填写本机控制 API")
		}
	}
	return &mihomoConnection{settings: opts, raw: raw, base: base, client: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *mihomoConnection) request(ctx context.Context, method, path string, body any, result any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.settings.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.settings.Secret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return errors.New("连接 Mihomo 控制 API 失败，请检查地址、密钥及运行状态")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Mihomo 控制 API %s %s 返回 HTTP %d", method, strings.Split(path, "?")[0], resp.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result); err != nil {
			return errors.New("Mihomo 控制 API 返回了无效 JSON")
		}
	}
	return nil
}

func (c *mihomoConnection) proxies(ctx context.Context) (map[string]mihomoProxy, error) {
	var result struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	err := c.request(ctx, http.MethodGet, "/proxies", nil, &result)
	return result.Proxies, err
}

func mihomoConcreteMembers(proxies map[string]mihomoProxy, group string) ([]string, error) {
	p, ok := proxies[group]
	if !ok || p.All == nil {
		return nil, errors.New("所选 Mihomo 分组不存在")
	}
	var names []string
	seen := map[string]bool{}
	for _, name := range p.All {
		node, exists := proxies[name]
		kind := strings.ToLower(node.Type)
		if !exists || node.All != nil || kind == "direct" || kind == "reject" || kind == "rejectdrop" || kind == "pass" || kind == "compatible" || seen[name] {
			continue
		}
		names = append(names, name)
		seen[name] = true
	}
	if len(names) == 0 {
		return nil, errors.New("所选分组没有具体代理节点（分组入口、直连及拒绝节点不参与轮询）")
	}
	return names, nil
}

func mihomoListenerPlan(raw map[string]any, names []string, portStart int) (map[string]any, []proxyNodeInput, error) {
	if len(names) > 1000 || portStart+len(names)-1 > 65535 {
		return nil, nil, errors.New("节点过多或超出端口范围")
	}
	// JSON roundtrip keeps the caller's original configuration intact for rollback.
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	var updated map[string]any
	if err := json.Unmarshal(data, &updated); err != nil {
		return nil, nil, err
	}
	listeners, _ := updated["listeners"].([]any)
	kept := []any{}
	used := map[int]bool{}
	for _, value := range listeners {
		listener, ok := value.(map[string]any)
		if !ok {
			return nil, nil, errors.New("已有 Mihomo listener 格式无效")
		}
		if strings.HasPrefix(strAny(listener["name"], ""), "nexora-register-") {
			continue
		}
		kept = append(kept, value)
		port := intAny(listener["port"], 0)
		if port == 0 {
			return nil, nil, errors.New("已有 listener 使用端口范围，请选择没有冲突的运行配置")
		}
		used[port] = true
	}
	for _, field := range []string{"port", "socks-port", "mixed-port", "redir-port", "tproxy-port"} {
		if port := intAny(updated[field], 0); port > 0 {
			used[port] = true
		}
	}
	var nodes []proxyNodeInput
	for i, name := range names {
		port := portStart + i
		if used[port] {
			return nil, nil, fmt.Errorf("注册出口端口 %d 与 Mihomo 现有入口冲突", port)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:12]
		kept = append(kept, map[string]any{"name": "nexora-register-" + hash, "type": "mixed", "listen": "127.0.0.1", "port": port, "proxy": name})
		nodes = append(nodes, proxyNodeInput{Name: name, URL: "http://127.0.0.1:" + strconv.Itoa(port)})
	}
	updated["listeners"] = kept
	return updated, nodes, nil
}

func (c *mihomoConnection) apply(ctx context.Context, before map[string]mihomoProxy, payload map[string]any) error {
	data, err := yaml.Marshal(payload)
	if err != nil {
		return err
	}
	if err := c.request(ctx, http.MethodPut, "/configs", map[string]string{"payload": string(data)}, nil); err != nil {
		return err
	}
	after, err := c.proxies(ctx)
	if err != nil {
		return err
	}
	// Config reload may restore cached selections. Preserve all current selectors.
	for name, state := range before {
		if state.Type != "Selector" || state.Now == "" || after[name].Now == state.Now {
			continue
		}
		if err := c.request(ctx, http.MethodPut, "/proxies/"+url.PathEscape(name), map[string]string{"name": state.Now}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleRegisterMihomo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var opts registerMihomoSettings
	if !readBody(w, r, &opts) {
		return
	}
	c, err := openRegisterMihomo(opts)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer c.client.CloseIdleConnections()
	proxies, err := c.proxies(r.Context())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.HasSuffix(r.URL.Path, "/groups") {
		type group struct {
			Name    string `json:"name"`
			Nodes   int    `json:"nodes"`
			Members int    `json:"members"`
		}
		groups := []group{}
		for name, p := range proxies {
			if p.All != nil {
				names, _ := mihomoConcreteMembers(proxies, name)
				groups = append(groups, group{name, len(names), len(p.All)})
			}
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
		// The controller secret remains server-side unless explicitly entered by the user.
		writeJSON(w, 200, map[string]any{"groups": groups, "controller": c.settings.Controller, "config_path": c.settings.ConfigPath})
		return
	}
	s.mihomoMu.Lock()
	defer s.mihomoMu.Unlock()
	s.register.mu.Lock()
	defer s.register.mu.Unlock()
	if s.register.cfg.Enabled || s.register.cfg.Stats.Running > 0 {
		writeErr(w, 409, "请等待当前注册或修复任务结束后配置 Mihomo")
		return
	}
	group, err := s.syncRegisterMihomo(r.Context(), c, proxies)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.register.cfg.Proxy = "group:" + group.ID
	if err := s.register.saveLocked(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"group": group, "proxy": s.register.cfg.Proxy})
}

func (s *Server) syncRegisterMihomo(ctx context.Context, c *mihomoConnection, proxies map[string]mihomoProxy) (ProxyGroup, error) {
	names, err := mihomoConcreteMembers(proxies, c.settings.Group)
	if err != nil {
		return ProxyGroup{}, err
	}
	// Keep live mode/port/TUN settings even when the GUI changed them since the
	// source file was generated. Controller configuration is deliberately kept.
	var live map[string]any
	if err := c.request(ctx, http.MethodGet, "/configs", nil, &live); err != nil {
		return ProxyGroup{}, err
	}
	for _, key := range []string{"port", "socks-port", "mixed-port", "redir-port", "tproxy-port", "mode", "log-level", "allow-lan", "bind-address", "ipv6", "tun", "tcp-concurrent", "sniffing", "interface-name", "find-process-mode"} {
		if value, ok := live[key]; ok {
			c.raw[key] = value
		}
	}
	payload, nodes, err := mihomoListenerPlan(c.raw, names, c.settings.PortStart)
	if err != nil {
		return ProxyGroup{}, err
	}
	var previous registerMihomoSettings
	if err := s.store.LoadDocument("register-mihomo.json", &previous); err != nil {
		return ProxyGroup{}, err
	}
	groups, err := s.store.LoadProxyGroups()
	if err != nil {
		return ProxyGroup{}, err
	}
	id := ""
	for _, group := range groups {
		if group.ID == previous.ProxyGroupID {
			id = group.ID
			for i := range nodes {
				for _, old := range group.Nodes {
					if old.Name == nodes[i].Name && old.URL == nodes[i].URL {
						nodes[i].ID = old.ID
					}
				}
			}
			break
		}
	}
	for _, node := range nodes {
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(node.URL, "http://"))
		previouslyOwned := false
		for _, group := range groups {
			if group.ID == previous.ProxyGroupID {
				for _, old := range group.Nodes {
					if old.URL == node.URL {
						previouslyOwned = true
					}
				}
			}
		}
		if !previouslyOwned {
			probe, err := net.Listen("tcp", "127.0.0.1:"+port)
			if err != nil {
				return ProxyGroup{}, fmt.Errorf("注册出口端口 %s 已被占用，请修改起始端口", port)
			}
			probe.Close()
		}
	}
	if err := writePrivateJSON(filepath.Join(s.dataDir, "mihomo-register-rollback.json"), c.raw); err != nil {
		return ProxyGroup{}, err
	}
	rollback := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.apply(cleanup, proxies, c.raw)
	}
	if err := c.apply(ctx, proxies, payload); err != nil {
		rollback()
		return ProxyGroup{}, err
	}
	for _, node := range nodes {
		address := strings.TrimPrefix(node.URL, "http://")
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err != nil {
			rollback()
			return ProxyGroup{}, errors.New("Mihomo 未成功创建注册专用出口，请检查监听端口及节点名称")
		}
		conn.Close()
	}
	group, err := s.store.SaveProxyGroup(proxyGroupInput{ID: id, Name: "注册 · Mihomo · " + c.settings.Group, Strategy: "round_robin", Nodes: nodes, Notes: "Mihomo 注册专用出口；不更改默认代理"})
	if err != nil {
		rollback()
		return ProxyGroup{}, err
	}
	c.settings.ProxyGroupID = group.ID
	if err := s.store.SaveDocument("register-mihomo.json", c.settings); err != nil {
		rollback()
		return ProxyGroup{}, err
	}
	return group, nil
}

// Called by the registration job before dispatching workers. Enabled is already
// true here, including automatic startup; HTTP configuration is locked out.
func (s *Server) ensureRegisterMihomo(ctx context.Context) error {
	return s.prepareRegisterMihomo(ctx, false)
}

func (s *Server) ensureAccountRepairMihomo(ctx context.Context) error {
	return s.prepareRegisterMihomo(ctx, true)
}

func (s *Server) prepareRegisterMihomo(ctx context.Context, reuseReady bool) error {
	if s.store == nil || s.register == nil {
		return nil
	}
	s.mihomoMu.Lock()
	defer s.mihomoMu.Unlock()
	var opts registerMihomoSettings
	if err := s.store.LoadDocument("register-mihomo.json", &opts); err != nil {
		return err
	}
	if opts.ProxyGroupID == "" || s.register.snapshot().Proxy != "group:"+opts.ProxyGroupID {
		return nil
	}
	if reuseReady {
		groups, err := s.store.LoadProxyGroups()
		if err != nil {
			return err
		}
		for _, group := range groups {
			if group.ID != opts.ProxyGroupID {
				continue
			}
			ready := len(group.Nodes) > 0
			for _, node := range group.Nodes {
				conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", strings.TrimPrefix(node.URL, "http://"))
				if err != nil {
					ready = false
					break
				}
				conn.Close()
			}
			if ready {
				return nil
			}
		}
		if cfg := s.register.snapshot(); (cfg.Enabled || cfg.Stats.Running > 0) && cfg.Stats.JobKind != "repair_abnormal" {
			return errors.New("Mihomo 注册出口不可用，请等待当前注册任务结束后重试修复")
		}
	}
	c, err := openRegisterMihomo(opts)
	if err != nil {
		return err
	}
	defer c.client.CloseIdleConnections()
	proxies, err := c.proxies(ctx)
	if err != nil {
		return err
	}
	_, err = s.syncRegisterMihomo(ctx, c, proxies)
	return err
}
