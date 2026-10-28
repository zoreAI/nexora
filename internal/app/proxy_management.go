package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type ProxyHealth struct {
	State     string `json:"state"`
	CheckedAt string `json:"checked_at,omitempty"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ProxyNode struct {
	ID                    string      `json:"id"`
	Name                  string      `json:"name"`
	URL                   string      `json:"url"`
	Enabled               bool        `json:"enabled"`
	ImageConcurrencyLimit int         `json:"image_concurrency_limit"`
	Notes                 string      `json:"notes"`
	Health                ProxyHealth `json:"health"`
	SuccessCount          int64       `json:"success_count"`
	FailCount             int64       `json:"fail_count"`
}

type ProxyGroup struct {
	ID                      string      `json:"id"`
	Name                    string      `json:"name"`
	Strategy                string      `json:"strategy"`
	RotationIntervalMinutes float64     `json:"rotation_interval_minutes"`
	Enabled                 bool        `json:"enabled"`
	Notes                   string      `json:"notes"`
	Nodes                   []ProxyNode `json:"nodes"`
	Health                  ProxyHealth `json:"health"`
	CreatedAt               string      `json:"created_at,omitempty"`
	UpdatedAt               string      `json:"updated_at,omitempty"`
}

type proxyNodeInput struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	URL                   string `json:"url"`
	Enabled               *bool  `json:"enabled"`
	ImageConcurrencyLimit int    `json:"image_concurrency_limit"`
	Notes                 string `json:"notes"`
}

type proxyGroupInput struct {
	ID                      string           `json:"id"`
	Name                    string           `json:"name"`
	Strategy                string           `json:"strategy"`
	RotationIntervalMinutes float64          `json:"rotation_interval_minutes"`
	Enabled                 *bool            `json:"enabled"`
	Notes                   string           `json:"notes"`
	Nodes                   []proxyNodeInput `json:"nodes"`
}

type proxyReference struct {
	Mode    string `json:"mode"`
	GroupID string `json:"group_id"`
	URL     string `json:"url"`
}

type proxyDefaults struct {
	Default  proxyReference
	Fallback *proxyReference
}

type proxyRoute struct {
	URL    string
	NodeID string
	Bound  bool
}

const proxyUnhealthyCooldown = 5 * time.Minute

func (s *Store) LoadProxyDefaults() (proxyDefaults, error) {
	var defaults proxyDefaults
	var mode, groupID, rawURL, fallbackMode, fallbackGroupID, fallbackURL string
	err := s.db.QueryRow("SELECT default_mode, default_group_id, default_url, fallback_mode, fallback_group_id, fallback_url FROM app_proxy_settings WHERE id=1").Scan(&mode, &groupID, &rawURL, &fallbackMode, &fallbackGroupID, &fallbackURL)
	if errors.Is(err, sql.ErrNoRows) {
		return defaults, nil
	}
	if err != nil {
		return defaults, err
	}
	defaults.Default = proxyReference{Mode: mode, GroupID: groupID, URL: rawURL}
	if fallbackMode != "" {
		defaults.Fallback = &proxyReference{Mode: fallbackMode, GroupID: fallbackGroupID, URL: fallbackURL}
	}
	return defaults, nil
}

func (s *Store) SaveProxyDefaults(defaultRef proxyReference, fallback *proxyReference) error {
	if defaultRef.Mode == "" {
		defaultRef.Mode = "custom"
	}
	var err error
	defaultRef, err = s.validateProxyReference(defaultRef)
	if err != nil {
		return fmt.Errorf("默认出口：%w", err)
	}
	var fallbackMode, fallbackGroupID, fallbackURL string
	if fallback != nil {
		validated, err := s.validateProxyReference(*fallback)
		if err != nil {
			return fmt.Errorf("备用出口：%w", err)
		}
		if defaultRef.Mode == "group" && validated.Mode == "group" && defaultRef.GroupID == validated.GroupID {
			return errors.New("备用代理组不能与默认代理组相同")
		}
		fallbackMode, fallbackGroupID, fallbackURL = validated.Mode, validated.GroupID, validated.URL
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockCollectionRows(s, tx, "proxy_references"); err != nil {
		return err
	}
	for _, id := range []string{defaultRef.GroupID, fallbackGroupID} {
		if id != "" {
			if err := s.ensureProxyGroupExistsTx(tx, id); err != nil {
				return err
			}
		}
	}
	query := "INSERT INTO app_proxy_settings (id, default_mode, default_group_id, default_url, fallback_mode, fallback_group_id, fallback_url, updated_at) VALUES (1," + s.bind(1) + "," + s.bind(2) + "," + s.bind(3) + "," + s.bind(4) + "," + s.bind(5) + "," + s.bind(6) + "," + s.bind(7) + ") ON CONFLICT (id) DO UPDATE SET default_mode=EXCLUDED.default_mode, default_group_id=EXCLUDED.default_group_id, default_url=EXCLUDED.default_url, fallback_mode=EXCLUDED.fallback_mode, fallback_group_id=EXCLUDED.fallback_group_id, fallback_url=EXCLUDED.fallback_url, updated_at=EXCLUDED.updated_at"
	if _, err := tx.Exec(query, defaultRef.Mode, defaultRef.GroupID, defaultRef.URL, fallbackMode, fallbackGroupID, fallbackURL, nowISO()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ensureProxyGroupExistsTx(tx *sql.Tx, id string) error {
	var exists int
	if err := tx.QueryRow("SELECT COUNT(*) FROM app_proxy_groups WHERE id="+s.bind(1), id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return errors.New("代理组不存在")
	}
	return nil
}

func (s *Store) validateProxyReference(ref proxyReference) (proxyReference, error) {
	ref.GroupID, ref.URL = strings.TrimSpace(ref.GroupID), strings.TrimSpace(ref.URL)
	switch ref.Mode {
	case "direct":
		if ref.GroupID != "" || ref.URL != "" {
			return ref, errors.New("直连不能包含代理组或地址")
		}
	case "custom":
		if ref.GroupID != "" {
			return ref, errors.New("自定义代理不能包含代理组")
		}
		if ref.URL != "" {
			urlValue, err := normalizeProxyURL(ref.URL)
			if err != nil {
				return ref, err
			}
			ref.URL = urlValue
		}
	case "group":
		if ref.GroupID == "" || ref.URL != "" {
			return ref, errors.New("代理组引用需要 group_id 且不能包含地址")
		}
		var exists int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM app_proxy_groups WHERE id="+s.bind(1), ref.GroupID).Scan(&exists); err != nil {
			return ref, err
		}
		if exists == 0 {
			return ref, errors.New("代理组不存在")
		}
	default:
		return ref, errors.New("代理引用模式无效")
	}
	return ref, nil
}

func (s *Server) selectedProxy() (string, error) {
	route, err := s.selectedProxyRoute()
	return route.URL, err
}

func (s *Server) selectedProxyRoute() (proxyRoute, error) {
	if s.store == nil {
		return proxyRoute{URL: strings.TrimSpace(s.cfg.Proxy)}, nil
	}
	defaults, err := s.store.LoadProxyDefaults()
	if err != nil {
		return proxyRoute{}, fmt.Errorf("读取代理配置失败：%w", err)
	}
	if defaults.Default.Mode == "" {
		return proxyRoute{URL: strings.TrimSpace(s.cfg.Proxy)}, nil
	}
	route, err := s.resolveProxyRoute(defaults.Default)
	if err != nil && defaults.Fallback != nil {
		fallbackRoute, fallbackErr := s.resolveProxyRoute(*defaults.Fallback)
		if fallbackErr == nil {
			return fallbackRoute, nil
		}
		return proxyRoute{}, fmt.Errorf("默认出口：%v；备用出口：%w", err, fallbackErr)
	}
	return route, err
}

func (s *Server) resolveProxyReference(ref proxyReference) (string, error) {
	route, err := s.resolveProxyRoute(ref)
	return route.URL, err
}

func (s *Server) resolveProxyRoute(ref proxyReference) (proxyRoute, error) {
	switch ref.Mode {
	case "direct":
		return proxyRoute{}, nil
	case "custom":
		return proxyRoute{URL: strings.TrimSpace(ref.URL)}, nil
	case "group":
		groups, err := s.store.LoadProxyGroups()
		if err != nil {
			return proxyRoute{}, err
		}
		for _, group := range groups {
			if group.ID != ref.GroupID || !group.Enabled {
				continue
			}
			nodes := make([]ProxyNode, 0, len(group.Nodes))
			for _, node := range group.Nodes {
				if node.Enabled && strings.TrimSpace(node.URL) != "" && !recentlyUnhealthyProxyNode(node, time.Now()) {
					nodes = append(nodes, node)
				}
			}
			if len(nodes) == 0 {
				return proxyRoute{}, fmt.Errorf("代理组 %q 没有可用节点", ref.GroupID)
			}
			index := 0
			switch group.Strategy {
			case "request_random":
				index = int(time.Now().UnixNano() % int64(len(nodes)))
			case "time_window":
				window := group.RotationIntervalMinutes
				if window <= 0 {
					window = 1
				}
				windowSeconds := max(int64(window*60), int64(1))
				index = int((time.Now().Unix() / windowSeconds) % int64(len(nodes)))
			default:
				index = int(atomic.AddUint64(&s.proxyCursor, 1)-1) % len(nodes)
			}
			return proxyRoute{URL: nodes[index].URL, NodeID: nodes[index].ID}, nil
		}
		return proxyRoute{}, fmt.Errorf("代理组 %q 不存在或已禁用", ref.GroupID)
	}
	return proxyRoute{}, errors.New("代理引用模式无效")
}

func recentlyUnhealthyProxyNode(node ProxyNode, now time.Time) bool {
	if node.Health.State != "unhealthy" {
		return false
	}
	checkedAt, err := time.Parse(time.RFC3339Nano, node.Health.CheckedAt)
	return err == nil && !checkedAt.After(now) && now.Sub(checkedAt) < proxyUnhealthyCooldown
}

func (s *Server) newUpstreamClientForAccount(account Account) (*UpstreamClient, error) {
	route, err := s.selectedProxyRouteForAccount(account)
	if err != nil {
		return nil, err
	}
	return s.newUpstreamClientForRoute(account, route)
}

func (s *Server) newUpstreamClientForRoute(account Account, route proxyRoute) (*UpstreamClient, error) {
	client, err := NewUpstreamClientForAccount(account, route.URL, s.ensureCurlImpersonateBinary, s.cfg)
	if err != nil {
		return nil, redactProxyTransportError(err, route.URL)
	}
	client.proxyRoute = route
	if route.URL != "" {
		client.client = &proxyHealthDoer{next: client.client, store: s.store, route: route}
	}
	return client, nil
}

func (s *Server) newFallbackUpstreamClientForAccount(account Account, failedRoute proxyRoute) (*UpstreamClient, error) {
	if s.store == nil || strings.TrimSpace(account.Proxy) != "" || failedRoute.Bound {
		return nil, nil
	}
	defaults, err := s.store.LoadProxyDefaults()
	if err != nil || defaults.Fallback == nil {
		return nil, err
	}
	route, err := s.resolveProxyRoute(*defaults.Fallback)
	if err != nil {
		return nil, err
	}
	if route.URL == failedRoute.URL {
		return nil, nil
	}
	return s.newUpstreamClientForRoute(account, route)
}

func (s *Server) newGroupSiblingUpstreamClientForAccount(account Account, failedRoute proxyRoute, attempted map[string]bool) (*UpstreamClient, error) {
	if s.store == nil || failedRoute.NodeID == "" {
		return nil, nil
	}
	groups, err := s.store.LoadProxyGroups()
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		if !group.Enabled {
			continue
		}
		belongs := false
		for _, node := range group.Nodes {
			if node.ID == failedRoute.NodeID {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		for _, node := range group.Nodes {
			if node.ID == failedRoute.NodeID || !node.Enabled || node.URL == "" || attempted[node.URL] || recentlyUnhealthyProxyNode(node, time.Now()) {
				continue
			}
			return s.newUpstreamClientForRoute(account, proxyRoute{URL: node.URL, NodeID: node.ID, Bound: failedRoute.Bound})
		}
		break
	}
	return nil, nil
}

func (s *Server) selectedProxyForAccount(account Account) (string, error) {
	route, err := s.selectedProxyRouteForAccount(account)
	return route.URL, err
}

func (s *Server) selectedProxyRouteForAccount(account Account) (proxyRoute, error) {
	assignment := strings.TrimSpace(account.Proxy)
	if assignment == "" {
		if s.store != nil && strings.TrimSpace(account.GroupID) != "" {
			groups, err := s.store.LoadAccountGroups()
			if err != nil {
				return proxyRoute{}, err
			}
			for _, group := range groups {
				if group.ID == account.GroupID && group.Enabled && group.Proxy != "" {
					assignment = group.Proxy
					break
				}
			}
		}
		if assignment == "" {
			return s.selectedProxyRoute()
		}
	}
	if assignment == "direct" {
		return proxyRoute{Bound: true}, nil
	}
	if strings.HasPrefix(assignment, "group:") {
		if s.store == nil {
			return proxyRoute{}, errors.New("账号代理组需要数据库存储")
		}
		route, err := s.resolveProxyRoute(proxyReference{Mode: "group", GroupID: strings.TrimPrefix(assignment, "group:")})
		route.Bound = true
		return route, err
	}
	urlValue, err := normalizeProxyURL(assignment)
	return proxyRoute{URL: urlValue, Bound: true}, err
}

func (s *Store) normalizeAccountProxyAssignment(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "direct" {
		return raw, nil
	}
	if strings.HasPrefix(raw, "group:") {
		id := strings.TrimSpace(strings.TrimPrefix(raw, "group:"))
		if id == "" {
			return "", errors.New("账号代理组 ID 不能为空")
		}
		groups, err := s.LoadProxyGroups()
		if err != nil {
			return "", err
		}
		for _, group := range groups {
			if group.ID != id {
				continue
			}
			if group.Enabled {
				for _, node := range group.Nodes {
					if node.Enabled && node.URL != "" {
						return "group:" + id, nil
					}
				}
			}
			return "", errors.New("账号代理组没有启用节点")
		}
		return "", errors.New("账号代理组不存在")
	}
	return normalizeProxyURL(raw)
}

func normalizeProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if scheme, authority, ok := strings.Cut(raw, "://"); ok {
		lowerScheme := strings.ToLower(scheme)
		if lowerScheme == "socks" {
			lowerScheme = "socks5"
		}
		if lowerScheme == "socks5" || lowerScheme == "socks5h" {
			if !strings.ContainsAny(authority, "/?#@") && !strings.HasPrefix(authority, "[") {
				parts := strings.SplitN(authority, ":", 4)
				if len(parts) == 4 && parts[0] != "" && parts[1] != "" && parts[2] != "" && parts[3] != "" {
					raw = lowerScheme + "://" + url.UserPassword(parts[2], parts[3]).String() + "@" + parts[0] + ":" + parts[1]
				} else {
					raw = lowerScheme + "://" + authority
				}
			} else {
				raw = lowerScheme + "://" + authority
			}
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme == "" || u.Hostname() == "" {
		return "", errors.New("代理地址必须是完整 URL，例如 http://127.0.0.1:7890")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("不支持的代理协议 %q，仅支持 http、https、socks5、socks5h", scheme)
	}
	if u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("代理地址不能包含路径、查询参数或片段")
	}
	if port := u.Port(); port != "" {
		for _, c := range port {
			if c < '0' || c > '9' {
				return "", errors.New("代理端口无效")
			}
		}
		var n int
		if _, err := fmt.Sscanf(port, "%d", &n); err != nil || n < 1 || n > 65535 {
			return "", errors.New("代理端口必须在 1 到 65535 之间")
		}
	} else if strings.Contains(u.Host, ":") && !strings.HasPrefix(u.Host, "[") {
		return "", errors.New("代理地址中的端口无效")
	}
	u.Scheme = scheme
	u.Path = ""
	return u.String(), nil
}

func (s *Store) LoadProxyGroups() ([]ProxyGroup, error) {
	rows, err := s.db.Query("SELECT id, name, strategy, rotation_interval_minutes, enabled, notes, created_at, updated_at FROM app_proxy_groups ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]ProxyGroup, 0)
	index := make(map[string]int)
	for rows.Next() {
		var group ProxyGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.Strategy, &group.RotationIntervalMinutes, &group.Enabled, &group.Notes, &group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, err
		}
		group.Nodes = []ProxyNode{}
		group.Health = ProxyHealth{State: "unknown"}
		index[group.ID] = len(groups)
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	nodes, err := s.db.Query("SELECT id, group_id, name, url, enabled, image_concurrency_limit, notes, health_state, checked_at, latency_ms, health_error, success_count, fail_count FROM app_proxy_nodes ORDER BY group_id, ordinal, id")
	if err != nil {
		return nil, err
	}
	defer nodes.Close()
	for nodes.Next() {
		var node ProxyNode
		var groupID, checkedAt, healthError string
		var latency sql.NullInt64
		if err := nodes.Scan(&node.ID, &groupID, &node.Name, &node.URL, &node.Enabled, &node.ImageConcurrencyLimit, &node.Notes, &node.Health.State, &checkedAt, &latency, &healthError, &node.SuccessCount, &node.FailCount); err != nil {
			return nil, err
		}
		node.Health.CheckedAt = checkedAt
		node.Health.Error = healthError
		if latency.Valid {
			node.Health.LatencyMS = &latency.Int64
		}
		if i, ok := index[groupID]; ok {
			groups[i].Nodes = append(groups[i].Nodes, node)
		}
	}
	if err := nodes.Err(); err != nil {
		return nil, err
	}
	for i := range groups {
		groups[i].Health = aggregateProxyHealth(groups[i].Nodes)
	}
	return groups, nil
}

func aggregateProxyHealth(nodes []ProxyNode) ProxyHealth {
	healthy, unknown := false, false
	enabled := false
	now := time.Now()
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		enabled = true
		switch node.Health.State {
		case "healthy":
			healthy = true
		case "unknown":
			unknown = true
		case "unhealthy":
			if !recentlyUnhealthyProxyNode(node, now) {
				unknown = true
			}
		}
	}
	if healthy {
		return ProxyHealth{State: "healthy"}
	}
	if unknown {
		return ProxyHealth{State: "unknown"}
	}
	if enabled {
		return ProxyHealth{State: "unhealthy"}
	}
	return ProxyHealth{State: "unknown"}
}

func (s *Store) SaveProxyGroup(input proxyGroupInput) (ProxyGroup, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Strategy = strings.TrimSpace(input.Strategy)
	if input.Name == "" {
		return ProxyGroup{}, errors.New("代理组名称不能为空")
	}
	if input.Strategy == "" {
		input.Strategy = "round_robin"
	}
	if input.Strategy != "request_random" && input.Strategy != "time_window" && input.Strategy != "round_robin" {
		return ProxyGroup{}, errors.New("代理组策略仅支持 request_random、time_window、round_robin")
	}
	if input.RotationIntervalMinutes < 0 || input.RotationIntervalMinutes > 1440 {
		return ProxyGroup{}, errors.New("轮换间隔必须在 0 到 1440 分钟之间")
	}
	if len(input.Nodes) == 0 {
		return ProxyGroup{}, errors.New("代理组至少需要一个节点")
	}
	if input.ID == "" {
		input.ID = "pg_" + randID(8)
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	seen := map[string]bool{}
	keepIDs := map[string]bool{}
	now := nowISO()
	tx, err := s.db.Begin()
	if err != nil {
		return ProxyGroup{}, err
	}
	defer tx.Rollback()
	var createdAt string
	err = tx.QueryRow("SELECT created_at FROM app_proxy_groups WHERE id = "+s.bind(1), input.ID).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		createdAt = now
	} else if err != nil {
		return ProxyGroup{}, err
	}
	query := "INSERT INTO app_proxy_groups (id, name, strategy, rotation_interval_minutes, enabled, notes, created_at, updated_at) VALUES (" + s.bind(1) + "," + s.bind(2) + "," + s.bind(3) + "," + s.bind(4) + "," + s.bind(5) + "," + s.bind(6) + "," + s.bind(7) + "," + s.bind(8) + ") ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, strategy=EXCLUDED.strategy, rotation_interval_minutes=EXCLUDED.rotation_interval_minutes, enabled=EXCLUDED.enabled, notes=EXCLUDED.notes, updated_at=EXCLUDED.updated_at"
	if _, err := tx.Exec(query, input.ID, input.Name, input.Strategy, input.RotationIntervalMinutes, enabled, strings.TrimSpace(input.Notes), createdAt, now); err != nil {
		return ProxyGroup{}, err
	}
	for ordinal, item := range input.Nodes {
		urlValue, err := normalizeProxyURL(item.URL)
		if err != nil {
			return ProxyGroup{}, fmt.Errorf("节点 %d：%w", ordinal+1, err)
		}
		if seen[urlValue] {
			return ProxyGroup{}, fmt.Errorf("代理组内存在重复节点（节点 %d）", ordinal+1)
		}
		seen[urlValue] = true
		if item.ImageConcurrencyLimit < 0 || item.ImageConcurrencyLimit > 10000 {
			return ProxyGroup{}, fmt.Errorf("节点 %d 图片并发限制必须在 0 到 10000 之间", ordinal+1)
		}
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = "pn_" + randID(8)
		} else {
			var owner string
			err := tx.QueryRow("SELECT group_id FROM app_proxy_nodes WHERE id="+s.bind(1), id).Scan(&owner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return ProxyGroup{}, err
			}
			if err == nil && owner != input.ID {
				return ProxyGroup{}, fmt.Errorf("节点 %q 属于其他代理组", id)
			}
		}
		if keepIDs[id] {
			return ProxyGroup{}, fmt.Errorf("重复节点 ID %q", id)
		}
		keepIDs[id] = true
		name := strings.TrimSpace(item.Name)
		if name == "" {
			parsed, _ := url.Parse(urlValue)
			name = parsed.Host
		}
		nodeEnabled := true
		if item.Enabled != nil {
			nodeEnabled = *item.Enabled
		}
		nodeQuery := "INSERT INTO app_proxy_nodes (id, group_id, name, url, enabled, image_concurrency_limit, notes, ordinal) VALUES (" + s.bind(1) + "," + s.bind(2) + "," + s.bind(3) + "," + s.bind(4) + "," + s.bind(5) + "," + s.bind(6) + "," + s.bind(7) + "," + s.bind(8) + ") ON CONFLICT (id) DO UPDATE SET group_id=EXCLUDED.group_id, name=EXCLUDED.name, url=EXCLUDED.url, enabled=EXCLUDED.enabled, image_concurrency_limit=EXCLUDED.image_concurrency_limit, notes=EXCLUDED.notes, ordinal=EXCLUDED.ordinal, health_state=CASE WHEN app_proxy_nodes.url<>EXCLUDED.url THEN 'unknown' ELSE app_proxy_nodes.health_state END, checked_at=CASE WHEN app_proxy_nodes.url<>EXCLUDED.url THEN '' ELSE app_proxy_nodes.checked_at END, latency_ms=CASE WHEN app_proxy_nodes.url<>EXCLUDED.url THEN NULL ELSE app_proxy_nodes.latency_ms END, health_error=CASE WHEN app_proxy_nodes.url<>EXCLUDED.url THEN '' ELSE app_proxy_nodes.health_error END, success_count=CASE WHEN app_proxy_nodes.url<>EXCLUDED.url THEN 0 ELSE app_proxy_nodes.success_count END, fail_count=CASE WHEN app_proxy_nodes.url<>EXCLUDED.url THEN 0 ELSE app_proxy_nodes.fail_count END"
		if _, err := tx.Exec(nodeQuery, id, input.ID, name, urlValue, nodeEnabled, item.ImageConcurrencyLimit, strings.TrimSpace(item.Notes), ordinal); err != nil {
			return ProxyGroup{}, err
		}
	}
	rows, err := tx.Query("SELECT id FROM app_proxy_nodes WHERE group_id = "+s.bind(1), input.ID)
	if err != nil {
		return ProxyGroup{}, err
	}
	var remove []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ProxyGroup{}, err
		}
		if !keepIDs[id] {
			remove = append(remove, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ProxyGroup{}, err
	}
	rows.Close()
	for _, id := range remove {
		if _, err := tx.Exec("DELETE FROM app_proxy_nodes WHERE id = "+s.bind(1), id); err != nil {
			return ProxyGroup{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ProxyGroup{}, err
	}
	groups, err := s.LoadProxyGroups()
	if err != nil {
		return ProxyGroup{}, err
	}
	for _, group := range groups {
		if group.ID == input.ID {
			return group, nil
		}
	}
	return ProxyGroup{}, errors.New("保存代理组后未能读取")
}

func (s *Store) DeleteProxyGroup(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id = strings.TrimSpace(id)
	if err := lockCollectionRows(s, tx, accountRows.name); err != nil {
		return err
	}
	if err := lockCollectionRows(s, tx, "proxy_references"); err != nil {
		return err
	}
	var inUse int
	if err := tx.QueryRow("SELECT COUNT(*) FROM app_proxy_settings WHERE default_group_id="+s.bind(1)+" OR fallback_group_id="+s.bind(2), id, id).Scan(&inUse); err != nil {
		return err
	}
	if inUse > 0 {
		return errors.New("代理组正在被默认或备用出口使用")
	}
	if err := tx.QueryRow("SELECT COUNT(*) FROM app_account_groups WHERE proxy="+s.bind(1), "group:"+id).Scan(&inUse); err != nil {
		return err
	}
	if inUse > 0 {
		return errors.New("代理组正在被账号组使用")
	}
	accounts, _, err := readCollectionRows[Account](tx, accountRows)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.Proxy == "group:"+id {
			return errors.New("代理组正在被账号使用")
		}
	}
	if _, err := tx.Exec("DELETE FROM app_proxy_nodes WHERE group_id = "+s.bind(1), id); err != nil {
		return err
	}
	result, err := tx.Exec("DELETE FROM app_proxy_groups WHERE id = "+s.bind(1), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) setProxyNodeHealth(id, state string, latency *int64, healthErr string) error {
	if state != "healthy" && state != "unhealthy" {
		return errors.New("invalid proxy health state")
	}
	_, err := s.db.Exec("UPDATE app_proxy_nodes SET health_state="+s.bind(1)+", checked_at="+s.bind(2)+", latency_ms="+s.bind(3)+", health_error="+s.bind(4)+", success_count=success_count+CASE WHEN "+s.bind(5)+"='healthy' THEN 1 ELSE 0 END, fail_count=fail_count+CASE WHEN "+s.bind(6)+"='unhealthy' THEN 1 ELSE 0 END WHERE id="+s.bind(7), state, time.Now().UTC().Format(time.RFC3339Nano), latency, healthErr, state, state, id)
	return err
}

type proxyNodeHealthUpdate struct {
	NodeID    string
	URL       string
	State     string
	LatencyMS *int64
	Error     string
}

func (s *Store) setProxyNodeHealthBatch(ctx context.Context, updates []proxyNodeHealthUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	checkedAt := time.Now().UTC().Format(time.RFC3339Nano)
	query := "UPDATE app_proxy_nodes SET health_state=" + s.bind(1) + ", checked_at=" + s.bind(2) + ", latency_ms=" + s.bind(3) + ", health_error=" + s.bind(4) + ", success_count=success_count+CASE WHEN " + s.bind(5) + "='healthy' THEN 1 ELSE 0 END, fail_count=fail_count+CASE WHEN " + s.bind(6) + "='unhealthy' THEN 1 ELSE 0 END WHERE id=" + s.bind(7) + " AND url=" + s.bind(8)
	for _, update := range updates {
		if update.State != "healthy" && update.State != "unhealthy" {
			return errors.New("invalid proxy health state")
		}
		if _, err := tx.ExecContext(ctx, query, update.State, checkedAt, update.LatencyMS, update.Error, update.State, update.State, update.NodeID, update.URL); err != nil {
			return err
		}
	}
	return tx.Commit()
}
