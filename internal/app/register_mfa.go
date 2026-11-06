package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const registerMFABase = "https://chatgpt.com/backend-api/accounts/mfa"

func writePrivateJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mfa-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Store) updateAccountMFA(token, status, secret string) error {
	return s.UpdateAccounts(func(items []Account) []Account {
		for i := range items {
			if items[i].AccessToken == token {
				items[i].MFAStatus = status
				if (status == "failed" || status == "pending") && items[i].Status == "正常" {
					items[i].Status = "异常"
				}
				if status == "failed" {
					items[i].MFASecret = nil
				} else if secret != "" {
					items[i].MFASecret = &secret
				}
				break
			}
		}
		return items
	})
}

func registerMFAAccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	return strings.TrimSpace(strAny(auth["chatgpt_account_id"], ""))
}

func registerTOTP(secret string, now time.Time) (string, error) {
	secret = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(secret, "="))
	if err != nil || len(key) == 0 {
		return "", errors.New("invalid TOTP secret")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 0x0f)
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000), nil
}

func freshRegisterTOTP(ctx context.Context, secret string) (string, time.Time, error) {
	now := time.Now()
	if remaining := 30 - now.Unix()%30; remaining < 6 {
		timer := time.NewTimer(time.Duration(remaining)*time.Second - time.Duration(now.Nanosecond()))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		case <-timer.C:
		}
		now = time.Now()
	}
	code, err := registerTOTP(secret, now)
	return code, now, err
}

func (s *Server) handleAccountMFACode(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if !readBody(w, r, &body) {
		return
	}
	for _, account := range s.store.LoadAccounts() {
		if account.AccessToken != body.AccessToken {
			continue
		}
		if account.MFAStatus != "enabled" || account.MFASecret == nil {
			writeErr(w, http.StatusConflict, "2FA is not enabled")
			return
		}
		code, now, err := freshRegisterTOTP(r.Context(), *account.MFASecret)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "invalid stored 2FA secret")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_in": 30 - now.Unix()%30})
		return
	}
	writeErr(w, http.StatusNotFound, "account not found")
}

func (s *Server) handleAccountMFASetup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if !readBody(w, r, &body) {
		return
	}
	var account *Account
	for _, item := range s.store.LoadAccounts() {
		if item.AccessToken == strings.TrimSpace(body.AccessToken) {
			account = &item
			break
		}
	}
	if account == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	releaseMFA, lockErr := s.accountMFALocks.acquire(r.Context(), accountOperationKey(ptrString(account.Email), account.AccessToken))
	if lockErr != nil {
		writeErr(w, http.StatusRequestTimeout, lockErr.Error())
		return
	}
	defer releaseMFA()
	for _, current := range s.store.LoadAccounts() {
		if current.AccessToken == account.AccessToken {
			account = &current
			break
		}
	}
	if account.MFAStatus != "failed" && account.MFAStatus != "pending" {
		writeErr(w, http.StatusConflict, "仅支持 2FA 设置失败或待确认的账号")
		return
	}
	if s.register == nil {
		writeErr(w, http.StatusServiceUnavailable, "注册服务不可用")
		return
	}
	route, err := s.register.resolveRegisterProxy(s.register.snapshot().Proxy)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	session, err := newRegisterSession(route.URL, 45, s.ensureCurlImpersonateBinary)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "创建 2FA 会话失败")
		return
	}
	defer session.close()
	session.proxyRoute = route
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	err = s.register.enableRegisterMFAWithFailover(ctx, session, account.AccessToken, uuid4())
	if err != nil {
		writeErr(w, http.StatusBadGateway, redactProxyTransportError(err, route.URL).Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *registerSession) registerMFARequest(ctx context.Context, method, path, accessToken, deviceID string, body any) (map[string]any, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + accessToken,
		"Accept":        "*/*",
		"Origin":        "https://chatgpt.com",
		"Referer":       "https://chatgpt.com/",
		"Oai-Did":       deviceID,
		"Oai-Language":  s.registerAcceptLanguage(),
		"Content-Type":  "application/json",
	}
	if accountID := registerMFAAccountID(accessToken); accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	resp, err := s.do(ctx, method, registerMFABase+path, headers, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Preserve actionable error codes without exposing an enrollment secret,
		// session ID or arbitrary upstream response content in logs.
		var payload struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Detail struct {
				Code string `json:"code"`
			} `json:"detail"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&payload)
		code := firstNonEmpty(payload.Error.Code, payload.Detail.Code)
		switch code {
		case "account_deactivated", "token_expired", "token_revoked", "token_invalidated", "unsupported_country_region_territory":
			return nil, fmt.Errorf("HTTP %d code=%s", resp.StatusCode, code)
		default:
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&result); err != nil {
		return nil, errors.New("invalid MFA response")
	}
	return result, nil
}

func isTransientRegisterNetworkError(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(err, target) {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Batch repair collects some upstream errors into strings before returning
	// them to the coordinator. Preserve failover for the common proxy failures
	// even after that wrapping has removed the original net.Error value. Do not
	// classify a bare context deadline as transient: mailbox OTP timeouts should
	// retain their recovery record instead of immediately retrying on every node.
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset by peer",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		": eof",
		"tls handshake timeout",
		"i/o timeout",
		// Region rejection is tied to the egress IP, not to the mailbox.
		// Treat it like a bad proxy node so a configured group can rotate.
		"unsupported_country_region_territory",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (s *registerService) enableRegisterMFA(ctx context.Context, session *registerSession, token, deviceID string) error {
	return s.enableRegisterMFAWithRequest(ctx, token, func(path string, body map[string]string) (map[string]any, error) {
		return session.registerMFARequest(ctx, http.MethodPost, path, token, deviceID, body)
	})
}

func (s *registerService) nextRegisterMFAProxy(proxyRef string, current proxyRoute, attempted map[string]bool) (proxyRoute, error) {
	if current.NodeID == "" || !strings.HasPrefix(proxyRef, "group:") {
		return proxyRoute{}, nil
	}
	groups, err := s.server.store.LoadProxyGroups()
	if err != nil {
		return proxyRoute{}, err
	}
	for _, group := range groups {
		if !group.Enabled || group.ID != strings.TrimPrefix(proxyRef, "group:") {
			continue
		}
		start := -1
		for i, node := range group.Nodes {
			if node.ID == current.NodeID {
				start = i
				break
			}
		}
		if start < 0 {
			return proxyRoute{}, nil
		}
		for step := 1; step < len(group.Nodes); step++ {
			node := group.Nodes[(start+step)%len(group.Nodes)]
			if node.Enabled && node.URL != "" && !attempted[node.URL] && !recentlyUnhealthyProxyNode(node, time.Now()) {
				return proxyRoute{URL: node.URL, NodeID: node.ID}, nil
			}
		}
		break
	}
	return proxyRoute{}, nil
}

func (s *registerService) enableRegisterMFAWithFailover(ctx context.Context, session *registerSession, token, deviceID string) error {
	current := session
	route := session.proxyRoute
	proxyRef := strings.TrimSpace(s.snapshot().Proxy)
	if selected, ok := ctx.Value(accountRepairProxyKey{}).(accountRepairProxy); ok {
		proxyRef = selected.ref
	}
	attempted := map[string]bool{route.URL: true}
	var enrolledSecret string
	defer func() {
		if current != session {
			current.close()
		}
	}()
	request := func(path string, body map[string]string) (map[string]any, error) {
		for {
			result, err := current.registerMFARequest(ctx, http.MethodPost, path, token, deviceID, body)
			if err == nil {
				if route.NodeID != "" {
					_ = s.server.store.recordProxyNodeRuntimeResult(route, true)
				}
				if path == "/enroll" {
					enrolledSecret = strings.TrimSpace(strAny(result["secret"], ""))
				}
				return result, nil
			}
			if ctx.Err() != nil || !isTransientRegisterNetworkError(err) || route.NodeID == "" {
				return nil, err
			}
			_ = s.server.store.recordProxyNodeRuntimeResult(route, false)
			s.appendLog(fmt.Sprintf("2FA 代理节点[%s] 连接失败，切换节点重试", route.NodeID), "yellow")
			newRoute, routeErr := s.nextRegisterMFAProxy(proxyRef, route, attempted)
			if routeErr != nil {
				return nil, fmt.Errorf("切换 2FA 代理节点: %w", routeErr)
			}
			if newRoute.NodeID == "" {
				return nil, fmt.Errorf("%w; 代理组无其他健康节点", err)
			}
			factory := s.mfaSessionFactory
			if factory == nil {
				factory = func(proxyURL string) (*registerSession, error) {
					return newRegisterSession(proxyURL, 45, s.server.ensureCurlImpersonateBinary)
				}
			}
			next, createErr := factory(newRoute.URL)
			if createErr != nil {
				return nil, fmt.Errorf("创建 2FA 代理会话: %w", createErr)
			}
			next.userAgent = current.userAgent
			next.proxyRoute = newRoute
			next.acceptLanguage = current.acceptLanguage
			next.secCHUA = current.secCHUA
			next.secCHUAArch = current.secCHUAArch
			next.secCHUAPlatform = current.secCHUAPlatform
			next.secCHUAPlatformVer = current.secCHUAPlatformVer
			next.browser = current.browser
			if current.client != nil && current.client.Jar != nil && next.client != nil && next.client.Jar != nil {
				for _, host := range []string{"auth.openai.com", "chatgpt.com"} {
					origin := &url.URL{Scheme: "https", Host: host}
					next.client.Jar.SetCookies(origin, current.client.Jar.Cookies(origin))
				}
			}
			if current != session {
				current.close()
			}
			current = next
			route = newRoute
			attempted[route.URL] = true
			if path == "/user/activate_enrollment" && enrolledSecret != "" {
				code, _, codeErr := freshRegisterTOTP(ctx, enrolledSecret)
				if codeErr != nil {
					return nil, codeErr
				}
				body["code"] = code
			}
		}
	}
	return s.enableRegisterMFAWithRequest(ctx, token, request)
}

func (s *registerService) enableRegisterMFAWithRequest(ctx context.Context, token string, request func(string, map[string]string) (map[string]any, error)) (resultErr error) {
	store := s.server.store
	defer func() {
		if isInvalidTokenErrorText(resultErr) || isAccountDeactivatedError(resultErr) {
			_ = s.server.recordReLoginFailure(token, resultErr)
		}
	}()
	setStatus := func(status, secret string) error { return store.updateAccountMFA(token, status, secret) }
	enrollment, err := request("/enroll", map[string]string{"factor_type": "totp", "source": "settings"})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			_ = setStatus("failed", "")
		}
		return fmt.Errorf("enroll: %w", err)
	}
	secret := strings.TrimSpace(strAny(enrollment["secret"], ""))
	sessionID := strings.TrimSpace(strAny(enrollment["session_id"], ""))
	if secret == "" || sessionID == "" {
		_ = setStatus("failed", "")
		return errors.New("enroll missing secret or session_id")
	}
	if err := setStatus("pending", secret); err != nil {
		return fmt.Errorf("save pending 2FA: %w", err)
	}
	code, _, err := freshRegisterTOTP(ctx, secret)
	if err != nil {
		return err
	}
	activation, err := request("/user/activate_enrollment", map[string]string{
		"code": code, "factor_type": "totp", "session_id": sessionID,
	})
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	if activation["success"] != true {
		return errors.New("activate returned success=false")
	}
	found := false
	if err := store.UpdateAccounts(func(items []Account) []Account {
		for i := range items {
			if items[i].AccessToken == token {
				items[i].MFAStatus = "enabled"
				items[i].MFASecret = &secret
				items[i].Status = "正常"
				found = true
				break
			}
		}
		return items
	}); err != nil {
		return fmt.Errorf("save enabled 2FA: %w", err)
	}
	if !found {
		return errors.New("save enabled 2FA: account not found")
	}
	return nil
}
