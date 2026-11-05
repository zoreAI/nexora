package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"golang.org/x/net/proxy"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"config": s.configMap(false)})
	case http.MethodPost:
		var body map[string]any
		if !readBody(w, r, &body) {
			return
		}
		if raw, ok := body["image_storage"]; ok {
			settings, err := parseImageStorageSettings(raw)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			body["image_storage"] = settings
		}
		for k, v := range body {
			s.cfg.Extra[k] = v
		}
		if v, ok := body["proxy"]; ok {
			proxy := strings.TrimSpace(strAny(v, ""))
			if proxy != "" {
				var err error
				proxy, err = normalizeProxyURL(proxy)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			if proxy != s.cfg.Proxy && s.store != nil {
				if err := s.store.SaveProxyDefaults(proxyReference{Mode: "custom", URL: proxy}, nil); err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			s.cfg.Proxy = proxy
		}
		if v, ok := body["sentinel_solver_url"]; ok {
			s.cfg.SentinelSolverURL = strings.TrimRight(strings.TrimSpace(strAny(v, "")), "/")
		}
		if v, ok := body["sentinel_solver_key"]; ok {
			s.cfg.SentinelSolverKey = strings.TrimSpace(strAny(v, ""))
		}
		if v, ok := body["base_url"]; ok {
			s.cfg.BaseURL = strings.TrimRight(strAny(v, ""), "/")
		}
		if v, ok := body["global_system_prompt"]; ok {
			s.cfg.GlobalSystemPrompt = strAny(v, "")
		}
		if v, ok := body["refresh_account_interval_minute"]; ok {
			s.cfg.RefreshAccountIntervalMinute = intAny(v, s.cfg.RefreshAccountIntervalMinute)
			if s.cfg.RefreshAccountIntervalMinute <= 0 {
				s.cfg.RefreshAccountIntervalMinute = int(defaultAccountRefreshInterval / time.Minute)
			}
			s.setAccountQuotaRefreshInterval(s.cfg.RefreshAccountIntervalMinute)
		}
		if v, ok := body["image_retention_days"]; ok {
			s.cfg.ImageRetentionDays = intAny(v, s.cfg.ImageRetentionDays)
		}
		if v, ok := body["image_poll_timeout_secs"]; ok {
			s.cfg.ImagePollTimeoutSecs = intAny(v, s.cfg.ImagePollTimeoutSecs)
		}
		if v, ok := body["image_poll_interval_secs"]; ok {
			s.cfg.ImagePollIntervalSecs = intAny(v, s.cfg.ImagePollIntervalSecs)
		}
		if v, ok := body["image_poll_initial_wait_secs"]; ok {
			s.cfg.ImagePollInitialWaitSecs = intAny(v, s.cfg.ImagePollInitialWaitSecs)
		}
		if v, ok := body["auto_remove_invalid_accounts"]; ok {
			s.cfg.AutoRemoveInvalidAccounts = boolAny(v, false)
		}
		if v, ok := body["auto_remove_rate_limited_accounts"]; ok {
			s.cfg.AutoRemoveRateLimitedAccounts = boolAny(v, false)
		}
		_ = s.saveConfig()
		writeJSON(w, 200, map[string]any{"config": s.configMap(false)})
	default:
		writeErr(w, 405, "method not allowed")
	}
}
func (s *Server) handleStorageInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	backend := map[string]any{"type": s.store.backend, "description": "SQL 数据库存储"}
	if s.store.backend == "sqlite" {
		backend["file_path"] = s.store.dsn
	}
	health := "healthy"
	if err := s.store.db.PingContext(r.Context()); err != nil {
		health = "unhealthy"
	}
	writeJSON(w, 200, map[string]any{"backend": backend, "health": map[string]any{"status": health, "backend": s.store.backend}})
}
func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	transport := "tls-client"
	bin := ""
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CHATGPT2API_UPSTREAM_TRANSPORT")), "curl-impersonate") {
		transport = "curl-impersonate"
		bin = strings.TrimSpace(os.Getenv("CHATGPT2API_CURL_IMPERSONATE_BIN"))
		if bin == "" {
			if p, err := s.ensureCurlImpersonateBinary(); err == nil {
				bin = p
			}
		}
	}
	binOK := false
	if bin != "" {
		if st, err := os.Stat(bin); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			binOK = true
		}
	}
	status := map[string]any{"ok": true, "version": s.version(), "storage": s.store.backend, "transport": transport, "curl_impersonate_bin": bin, "curl_impersonate_executable": binOK, "accounts": len(s.store.LoadAccounts()), "tasks": len(s.store.LoadTasks())}
	if s.imageTasks != nil {
		status["image_task_runner"] = s.imageTaskRunnerStatus()
	}
	writeJSON(w, 200, status)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method == http.MethodGet {
		proxy, err := s.selectedProxy()
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"proxy": map[string]any{"url": proxy, "enabled": proxy != ""}})
		return
	}
	if r.Method == http.MethodPost {
		var b map[string]any
		if !readBody(w, r, &b) {
			return
		}
		proxy := strings.TrimSpace(strAny(b["url"], strAny(b["proxy"], s.cfg.Proxy)))
		if proxy != "" {
			var err error
			proxy, err = normalizeProxyURL(proxy)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if s.store != nil {
			if err := s.store.SaveProxyDefaults(proxyReference{Mode: "custom", URL: proxy}, nil); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		s.cfg.Proxy = proxy
		if s.cfg.Extra == nil {
			s.cfg.Extra = map[string]any{}
		}
		s.cfg.Extra["proxy"] = proxy
		_ = s.saveConfig()
		writeJSON(w, 200, map[string]any{"proxy": map[string]any{"url": proxy, "enabled": proxy != ""}})
		return
	}
	writeErr(w, 405, "method not allowed")
}

func (s *Server) handleProxyView(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	groups, err := s.store.LoadProxyGroups()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取代理组失败："+err.Error())
		return
	}
	defaults, err := s.store.LoadProxyDefaults()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取代理默认配置失败："+err.Error())
		return
	}
	defaultRef := defaults.Default
	if defaultRef.Mode == "" {
		defaultRef = proxyReference{Mode: "custom", URL: s.cfg.Proxy}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": 1, "generated_at": nowISO(), "default_reference": defaultRef, "fallback_reference": defaults.Fallback, "groups": groups})
}

func (s *Server) handleProxyDefaults(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method == http.MethodGet {
		defaults, err := s.store.LoadProxyDefaults()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取代理默认配置失败："+err.Error())
			return
		}
		if defaults.Default.Mode == "" {
			defaults.Default = proxyReference{Mode: "custom", URL: s.cfg.Proxy}
		}
		writeJSON(w, http.StatusOK, map[string]any{"default_reference": defaults.Default, "fallback_reference": defaults.Fallback})
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		DefaultReference  proxyReference  `json:"default_reference"`
		FallbackReference *proxyReference `json:"fallback_reference"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if err := s.store.SaveProxyDefaults(body.DefaultReference, body.FallbackReference); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.DefaultReference.Mode == "custom" {
		defaults, err := s.store.LoadProxyDefaults()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.cfg.Proxy = defaults.Default.URL
		if s.cfg.Extra == nil {
			s.cfg.Extra = map[string]any{}
		}
		s.cfg.Extra["proxy"] = s.cfg.Proxy
		_ = s.saveConfig()
	}
	defaults, _ := s.store.LoadProxyDefaults()
	writeJSON(w, http.StatusOK, map[string]any{"default_reference": defaults.Default, "fallback_reference": defaults.Fallback})
}

func (s *Server) handleProxyGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method == http.MethodGet {
		groups, err := s.store.LoadProxyGroups()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取代理组失败："+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input proxyGroupInput
	if !readBody(w, r, &input) {
		return
	}
	group, err := s.store.SaveProxyGroup(input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": group})
}

func (s *Server) handleProxyGroupID(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/proxy/groups/"))
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, http.StatusBadRequest, "代理组 ID 无效")
		return
	}
	if err := s.store.DeleteProxyGroup(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "代理组不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "删除代理组失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted_id": id})
}

func (s *Server) handleProxyGroupTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID     string `json:"id"`
		NodeID string `json:"node_id"`
		URL    string `json:"url"`
	}
	if !readBody(w, r, &body) {
		return
	}
	type target struct{ nodeID, url string }
	targets := make([]target, 0)
	if rawURL := strings.TrimSpace(body.URL); rawURL != "" {
		targets = append(targets, target{url: rawURL})
	} else {
		if strings.TrimSpace(body.ID) == "" {
			writeErr(w, http.StatusBadRequest, "需要代理组 ID 或代理地址")
			return
		}
		groups, err := s.store.LoadProxyGroups()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取代理组失败："+err.Error())
			return
		}
		found := false
		for _, group := range groups {
			if group.ID != body.ID {
				continue
			}
			found = true
			for _, node := range group.Nodes {
				if node.Enabled && (body.NodeID == "" || node.ID == body.NodeID) {
					targets = append(targets, target{nodeID: node.ID, url: node.URL})
				}
			}
			break
		}
		if !found {
			writeErr(w, http.StatusNotFound, "代理组不存在")
			return
		}
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "没有找到可测试的启用节点")
		return
	}
	type item struct {
		NodeID string         `json:"node_id"`
		Result map[string]any `json:"result"`
	}
	results := make([]item, len(targets))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(targets), 8) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if r.Context().Err() != nil {
					return
				}
				results[index] = item{NodeID: targets[index].nodeID, Result: s.testProxyURL(r, targets[index].url)}
			}
		}()
	}
enqueue:
	for index := range targets {
		select {
		case <-r.Context().Done():
			break enqueue
		case jobs <- index:
		}
	}
	close(jobs)
	workers.Wait()
	if r.Context().Err() != nil {
		return
	}
	succeeded, failed := 0, 0
	maxLatency := int64(0)
	updates := make([]proxyNodeHealthUpdate, 0, len(results))
	for index, result := range results {
		if r.Context().Err() != nil {
			return
		}
		if result.Result["ok"] == true {
			succeeded++
		} else {
			failed++
		}
		if latency, ok := result.Result["latency_ms"].(int64); ok && latency > maxLatency {
			maxLatency = latency
		}
		if result.NodeID != "" {
			var latency *int64
			if value, ok := result.Result["latency_ms"].(int64); ok {
				latency = &value
			}
			state := "unhealthy"
			healthErr := strAny(result.Result["error"], "")
			if result.Result["ok"] == true {
				state, healthErr = "healthy", ""
			}
			updates = append(updates, proxyNodeHealthUpdate{NodeID: result.NodeID, URL: targets[index].url, State: state, LatencyMS: latency, Error: healthErr})
		}
	}
	if len(updates) > 0 {
		if err := s.store.setProxyNodeHealthBatch(r.Context(), updates); err != nil {
			if r.Context().Err() != nil {
				return
			}
			writeErr(w, http.StatusInternalServerError, "保存代理节点状态失败："+err.Error())
			return
		}
	}
	status := "failed"
	if succeeded == len(results) {
		status = "success"
	} else if succeeded > 0 {
		status = "partial"
	}
	message := fmt.Sprintf("测试完成：成功 %d，失败 %d", succeeded, failed)
	writeJSON(w, http.StatusOK, map[string]any{"summary": map[string]any{"status": status, "tone": map[string]string{"success": "success", "partial": "warning", "failed": "danger"}[status], "total": len(results), "succeeded": succeeded, "failed": failed, "max_latency_ms": maxLatency, "label": message, "message": message}, "results": results})
}

func (s *Server) testProxyURL(r *http.Request, raw string) map[string]any {
	started := time.Now()
	result := map[string]any{"ok": false, "status": 0, "latency_ms": int64(0), "error": nil}
	defer func() { result["latency_ms"] = time.Since(started).Milliseconds() }()
	normalized, err := normalizeProxyURL(raw)
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	proxyURL, _ := url.Parse(normalized)
	target := "https://chatgpt.com/backend-api/me"
	upstreamClient, err := newUpstreamHTTPClient(normalized, s.ensureCurlImpersonateBinary)
	if err != nil {
		result["error"] = redactProxyTransportError(err, normalized).Error()
		return result
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, target, nil)
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	req.Header.Set("User-Agent", upstreamUserAgent())
	resp, err := upstreamClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		result["status"] = resp.StatusCode
		result["ok"] = resp.StatusCode > 0 && resp.StatusCode < 500
		result["message"] = resp.Status
		if !result["ok"].(bool) {
			result["error"] = resp.Status
		}
		return result
	}

	// The ordinary HTTPS probe only distinguishes a target-specific failure.
	transport := &http.Transport{}
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		var auth *proxy.Auth
		if proxyURL.User != nil {
			auth = &proxy.Auth{User: proxyURL.User.Username()}
			auth.Password, _ = proxyURL.User.Password()
		}
		dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
		if err != nil {
			result["error"] = err.Error()
			return result
		}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.Dial(network, address)
		}
	} else {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	result["error"] = diagnoseProxyFailure(ctx, client, err, normalized)
	return result
}

func diagnoseProxyFailure(ctx context.Context, client *http.Client, upstreamErr error, proxyURL string) string {
	message := upstreamErr.Error()
	if parsed, err := url.Parse(proxyURL); err == nil && parsed.User != nil {
		message = strings.ReplaceAll(message, parsed.User.String()+"@", "***@")
		message = strings.ReplaceAll(message, parsed.User.String(), "***")
	}
	if strings.Contains(message, "407") || strings.Contains(strings.ToLower(message), "proxy authentication required") {
		return "代理认证失败（HTTP 407）"
	}
	if !strings.Contains(strings.ToLower(message), "reset") && !strings.Contains(strings.ToLower(message), "eof") {
		return message
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
	if err != nil {
		return message
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 500 {
			return "代理可访问普通 HTTPS，但连接 chatgpt.com 失败：" + message
		}
	}
	return message
}

func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !readBody(w, r, &body) {
		return
	}
	proxy := strings.TrimSpace(body.URL)
	if proxy == "" {
		var err error
		proxy, err = s.selectedProxy()
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": false, "status": 0, "latency_ms": int64(0), "error": err.Error()}})
			return
		}
	}
	if proxy == "" {
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": false, "status": 0, "latency_ms": int64(0), "error": "未配置代理地址"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": s.testProxyURL(r, proxy)})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	startDate := strings.TrimSpace(r.URL.Query().Get("start_date"))
	endDate := strings.TrimSpace(r.URL.Query().Get("end_date"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	endpoint := strings.TrimSpace(r.URL.Query().Get("endpoint"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	items := s.logSvc.listFiltered(typ, startDate, endDate, status, endpoint, model, query, 200)
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) handleLogsDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var b struct {
		IDs []string `json:"ids"`
	}
	if !readBody(w, r, &b) {
		return
	}
	removed := s.logSvc.delete(b.IDs)
	writeJSON(w, 200, map[string]any{"removed": removed})
}

func disabledBackupSettings() map[string]any {
	return map[string]any{"enabled": false, "provider": "disabled", "account_id": "", "access_key_id": "", "secret_access_key": "", "bucket": "", "prefix": "backups", "interval_minutes": 1440, "rotation_keep": 0, "encrypt": false, "passphrase": "", "include": map[string]any{"config": true, "register": false, "cpa": true, "sub2api": true, "logs": true, "image_tasks": true, "accounts_snapshot": true, "auth_keys_snapshot": true, "chat_conversations_snapshot": true, "images": false}}
}
func (s *Server) handleBackupDisabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"result": map[string]any{"ok": false, "status": 0, "message": "R2 备份已在 Go 精简版中禁用"}, "ok": false})
}
func (s *Server) handleBackupsDisabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"items": []any{}, "state": map[string]any{"running": false, "last_status": "disabled"}, "settings": disabledBackupSettings()})
}
func (s *Server) handleRegisterDisabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"register": map[string]any{"enabled": false, "running": false, "logs": []any{}, "message": "注册机已在 Go 版中移除"}})
}
func (s *Server) handleVideoMetadata(w http.ResponseWriter, r *http.Request) {
	meta, err := bilibiliMetadata(r.URL.Query().Get("url"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, meta)
}
func (s *Server) handleVideoCover(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("url"))
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		writeErr(w, 400, "url is required")
		return
	}
	host := strings.ToLower(parsed.Hostname())
	if !(strings.HasSuffix(host, "hdslb.com") || strings.HasSuffix(host, "bilibili.com") || strings.HasSuffix(host, "bilivideo.com")) {
		writeErr(w, 400, "unsupported cover host")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer resp.Body.Close()
	ct := strings.Split(resp.Header.Get("Content-Type"), ";")[0]
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !strings.HasPrefix(ct, "image/") {
		writeErr(w, 502, fmt.Sprintf("cover fetch failed: %d", resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", ct)
	_, _ = io.Copy(w, resp.Body)
}

func bilibiliMetadata(raw string) (map[string]any, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return nil, fmt.Errorf("url is required")
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("unsupported url")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "b23.tv" || host == "bili2233.cn" {
		client := &http.Client{Timeout: 15 * time.Second}
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
		resp, err := client.Do(req)
		if err == nil {
			u = resp.Request.URL.String()
			_ = resp.Body.Close()
			parsed, _ = url.Parse(u)
			host = strings.ToLower(parsed.Hostname())
		}
	}
	if !(host == "bilibili.com" || host == "www.bilibili.com" || host == "m.bilibili.com" || strings.HasSuffix(host, ".bilibili.com")) {
		return nil, fmt.Errorf("unsupported video host")
	}
	segs := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	videoID := ""
	for i := 0; i+1 < len(segs); i++ {
		if strings.ToLower(segs[i]) == "video" {
			videoID = segs[i+1]
			break
		}
	}
	if !regexp.MustCompile(`(?i)^(BV[0-9A-Za-z]+|av\d+)$`).MatchString(videoID) {
		return nil, fmt.Errorf("unsupported bilibili video id")
	}
	page := parsed.Query().Get("p")
	if page == "" {
		page = parsed.Query().Get("page")
	}
	if page == "" {
		page = "1"
	}
	apiURL := "https://api.bilibili.com/x/web-interface/view?"
	isAV := strings.HasPrefix(strings.ToLower(videoID), "av")
	if isAV {
		apiURL += "aid=" + url.QueryEscape(strings.TrimPrefix(strings.TrimPrefix(videoID, "av"), "AV"))
	} else {
		apiURL += "bvid=" + url.QueryEscape(videoID)
	}
	req, _ := http.NewRequest(http.MethodGet, apiURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if intAny(payload["code"], -1) != 0 {
		return nil, fmt.Errorf("%s", firstNonEmpty(strAny(payload["message"], ""), "bilibili metadata failed"))
	}
	data, _ := payload["data"].(map[string]any)
	bvid := strAny(data["bvid"], "")
	aid := strAny(data["aid"], "")
	canonical := firstNonEmpty(bvid, "av"+aid, videoID)
	embedQuery := "bvid=" + url.QueryEscape(bvid)
	if bvid == "" {
		embedQuery = "aid=" + url.QueryEscape(aid)
	}
	rawThumb := strAny(data["pic"], "")
	thumb := ""
	if rawThumb != "" {
		thumb = "/api/video/cover?url=" + url.QueryEscape(rawThumb)
	}
	return map[string]any{"provider": "bilibili", "id": canonical, "title": strAny(data["title"], ""), "thumb_url": thumb, "raw_thumb_url": rawThumb, "watch_url": "https://www.bilibili.com/video/" + canonical, "embed_url": "https://player.bilibili.com/player.html?" + embedQuery + "&p=" + url.QueryEscape(page) + "&autoplay=1"}, nil
}

func (s *Server) handleCPAPools(w http.ResponseWriter, r *http.Request) {
	s.handleNamedList(w, r, "cpa_pools.json", "pools", "pool")
}
func (s *Server) handleSub2APIServers(w http.ResponseWriter, r *http.Request) {
	s.handleNamedList(w, r, "sub2api_servers.json", "servers", "server")
}
func (s *Server) handleNamedList(w http.ResponseWriter, r *http.Request, file, listKey, itemKey string) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	items := s.store.LoadList(file)
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{listKey: items})
	case http.MethodPost:
		var b map[string]any
		if !readBody(w, r, &b) {
			return
		}
		b["id"] = randID(6)
		items = append(items, b)
		_ = s.store.SaveList(file, items)
		writeJSON(w, 200, map[string]any{itemKey: b, listKey: items})
	default:
		writeErr(w, 405, "method not allowed")
	}
}
func (s *Server) handleCPAPoolID(w http.ResponseWriter, r *http.Request) {
	s.handleNamedListID(w, r, "/api/cpa/pools/", "cpa_pools.json", "pools", "pool")
}
func (s *Server) handleSub2APIServerID(w http.ResponseWriter, r *http.Request) {
	s.handleNamedListID(w, r, "/api/sub2api/servers/", "sub2api_servers.json", "servers", "server")
}
func (s *Server) handleNamedListID(w http.ResponseWriter, r *http.Request, prefix, file, listKey, itemKey string) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	if len(parts) > 1 {
		switch parts[1] {
		case "files":
			writeJSON(w, 200, map[string]any{"pool_id": id, "files": []any{}})
			return
		case "import":
			writeJSON(w, 200, map[string]any{"import_job": nil})
			return
		case "groups":
			writeJSON(w, 200, map[string]any{"server_id": id, "groups": []any{}})
			return
		case "accounts":
			writeJSON(w, 200, map[string]any{"server_id": id, "accounts": []any{}})
			return
		}
	}
	items := s.store.LoadList(file)
	idx := -1
	for i, it := range items {
		if strAny(it["id"], "") == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, 404, "not found")
		return
	}
	if r.Method == http.MethodDelete {
		items = append(items[:idx], items[idx+1:]...)
		_ = s.store.SaveList(file, items)
		writeJSON(w, 200, map[string]any{listKey: items})
		return
	}
	if r.Method == http.MethodPost {
		var b map[string]any
		if !readBody(w, r, &b) {
			return
		}
		for k, v := range b {
			items[idx][k] = v
		}
		_ = s.store.SaveList(file, items)
		writeJSON(w, 200, map[string]any{itemKey: items[idx], listKey: items})
		return
	}
	writeErr(w, 405, "method not allowed")
}
