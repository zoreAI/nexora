package app

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	stdhttp "net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"golang.org/x/net/proxy"
)

const (
	registerAuthBase    = "https://auth.openai.com"
	registerPlatformURL = "https://platform.openai.com"
	outlookTokenURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	registerAuth0Client = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
)

type cloudMailTokenCacheItem struct {
	token     string
	expiresAt time.Time
}

var (
	cloudMailTokenMu    sync.Mutex
	cloudMailTokenCache = map[string]cloudMailTokenCacheItem{}
)

type registerSession struct {
	client               *stdhttp.Client
	curl                 upstreamHTTPDoer
	userAgent            string
	acceptLanguage       string
	secCHUA              string
	secCHUAArch          string
	secCHUAPlatform      string
	secCHUAPlatformVer   string
	accessFlowID         string
	sentinelSOToken      string
	browser              string
	lastAuthorizeLanding string
	registerOTPURL       string
	lastAuthorizeDebug   registerFlowDebug
	lastValidateOTPDebug registerFlowDebug
	lastCreateDebug      registerFlowDebug
}

type registerFlowDebug struct {
	ClientID             string
	RedirectURI          string
	ScreenHint           string
	Referer              string
	Flow                 string
	SOTokenPresent       bool
	SentinelTokenPresent bool
	AccessFlowIDPresent  bool
}

func (d registerFlowDebug) String() string {
	parts := []string{}
	if strings.TrimSpace(d.ClientID) != "" {
		parts = append(parts, "client_id="+strings.TrimSpace(d.ClientID))
	}
	if strings.TrimSpace(d.RedirectURI) != "" {
		parts = append(parts, "redirect_uri="+strings.TrimSpace(d.RedirectURI))
	}
	if strings.TrimSpace(d.ScreenHint) != "" {
		parts = append(parts, "screen_hint="+strings.TrimSpace(d.ScreenHint))
	}
	if strings.TrimSpace(d.Referer) != "" {
		parts = append(parts, "referer="+strings.TrimSpace(d.Referer))
	}
	if strings.TrimSpace(d.Flow) != "" {
		parts = append(parts, "flow="+strings.TrimSpace(d.Flow))
	}
	parts = append(parts,
		fmt.Sprintf("so_token_present=%t", d.SOTokenPresent),
		fmt.Sprintf("sentinel_token_present=%t", d.SentinelTokenPresent),
		fmt.Sprintf("access_flow_id_present=%t", d.AccessFlowIDPresent),
	)
	return strings.Join(parts, " ")
}

type registerMailbox struct {
	Provider      string
	ProviderRef   string
	Label         string
	Address       string
	Token         string
	Password      string
	ClientID      string
	AccountID     string
	RefreshToken  string
	MailboxName   string
	CodeNotBefore time.Time
	Extra         map[string]any
}

type registerProvider interface {
	Name() string
	CreateMailbox(username string) (*registerMailbox, error)
	WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error)
	MarkFailed(mailbox *registerMailbox, reason string)
	Session() registerProviderSession
	Close()
}

func newRegisterSession(proxy string, timeout int, binGetter func() (string, error)) (*registerSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport, err := newProxyHTTPTransport(proxy)
	if err != nil {
		return nil, err
	}
	client := &stdhttp.Client{
		Jar:       jar,
		Timeout:   time.Duration(maxInt(timeout, 30)) * time.Second,
		Transport: transport,
		CheckRedirect: func(req *stdhttp.Request, via []*stdhttp.Request) error {
			return nil
		},
	}
	fp := randomRegisterFingerprint()
	var curl upstreamHTTPDoer
	if binGetter != nil && !strings.EqualFold(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_CURL_IMPERSONATE")), "0") && !strings.EqualFold(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_CURL_IMPERSONATE")), "false") {
		binGetter = registerCurlBinaryGetter(binGetter, fp.browser)
		if c, err := newCurlImpersonateClient(proxy, binGetter); err == nil {
			curl = c
		}
	}
	return &registerSession{
		client:             client,
		curl:               curl,
		userAgent:          fp.userAgent,
		acceptLanguage:     fp.acceptLanguage,
		secCHUA:            fp.secCHUA,
		secCHUAArch:        fp.secCHUAArch,
		secCHUAPlatform:    fp.secCHUAPlatform,
		secCHUAPlatformVer: fp.secCHUAPlatformVer,
		accessFlowID:       uuid4(),
		sentinelSOToken:    strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_SENTINEL_SO_TOKEN")),
		browser:            fp.browser,
	}, nil
}

func newProxyHTTPTransport(rawProxy string) (*stdhttp.Transport, error) {
	transport := &stdhttp.Transport{}
	if strings.TrimSpace(rawProxy) == "" {
		return transport, nil
	}
	normalized, err := normalizeProxyURL(rawProxy)
	if err != nil {
		return nil, err
	}
	proxyURL, err := url.Parse(normalized)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h" {
		transport.Proxy = stdhttp.ProxyURL(proxyURL)
		return transport, nil
	}
	var auth *proxy.Auth
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("创建 SOCKS5 代理失败: %w", err)
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, address)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return dialer.Dial(network, address)
	}
	return transport, nil
}

type registerFingerprint struct {
	userAgent          string
	acceptLanguage     string
	secCHUA            string
	secCHUAArch        string
	secCHUAPlatform    string
	secCHUAPlatformVer string
	browser            string
}

func randomRegisterFingerprint() registerFingerprint {
	if ua := strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_USER_AGENT")); ua != "" {
		browser := strings.ToLower(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_BROWSER")))
		if browser != "chrome" {
			browser = "edge"
		}
		return registerFingerprint{
			userAgent:          ua,
			acceptLanguage:     firstNonEmpty(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_ACCEPT_LANGUAGE")), "en,zh-CN;q=0.9,zh-TW;q=0.8,zh;q=0.7"),
			secCHUA:            firstNonEmpty(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_SEC_CH_UA")), upstreamSecCHUA()),
			secCHUAArch:        firstNonEmpty(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_SEC_CH_UA_ARCH")), `"x86"`),
			secCHUAPlatform:    firstNonEmpty(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_SEC_CH_UA_PLATFORM")), `"macOS"`),
			secCHUAPlatformVer: firstNonEmpty(strings.TrimSpace(os.Getenv("CHATGPT2API_REGISTER_SEC_CH_UA_PLATFORM_VERSION")), `"15.7.4"`),
			browser:            browser,
		}
	}
	items := []registerFingerprint{
		{
			userAgent:          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
			acceptLanguage:     "en,zh-CN;q=0.9,zh-TW;q=0.8,zh;q=0.7",
			secCHUA:            `"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"`,
			secCHUAArch:        `"x86"`,
			secCHUAPlatform:    `"macOS"`,
			secCHUAPlatformVer: `"15.7.4"`,
			browser:            "chrome",
		},
	}
	return items[rand.Intn(len(items))]
}

func registerCurlBinaryGetter(binGetter func() (string, error), browser string) func() (string, error) {
	return func() (string, error) {
		bin, err := binGetter()
		if err != nil {
			return "", err
		}
		if preferred := findSiblingRegisterCurlBinary(bin, browser); preferred != "" {
			return preferred, nil
		}
		return bin, nil
	}
}

func findSiblingRegisterCurlBinary(bin, browser string) string {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return ""
	}
	names := []string{"curl_edge101"}
	if browser == "chrome" {
		names = []string{"curl-impersonate-chrome", "curl_chrome116", "curl_chrome110", "curl_chrome101"}
	}
	dir := filepath.Dir(bin)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	lowerBase := strings.ToLower(filepath.Base(bin))
	if browser == "chrome" && strings.Contains(lowerBase, "chrome") {
		return bin
	}
	if browser == "edge" && strings.Contains(lowerBase, "edge") {
		return bin
	}
	return ""
}

func registerFullVersionList(s *registerSession) string {
	if s != nil && strings.EqualFold(strings.TrimSpace(s.browser), "chrome") && strings.Contains(s.secCHUA, `v="150"`) {
		return `"Not;A=Brand";v="8.0.0.0", "Chromium";v="150.0.7871.182", "Google Chrome";v="150.0.7871.182"`
	}
	if s != nil && strings.EqualFold(strings.TrimSpace(s.browser), "edge") {
		return `"Microsoft Edge";v="143.0.0.0", "Chromium";v="143.0.0.0", "Not A(Brand";v="24.0.0.0"`
	}
	return `"Chromium";v="145.0.0.0", "Not:A-Brand";v="99.0.0.0", "Google Chrome";v="145.0.0.0"`
}

func registerFullVersion(s *registerSession) string {
	if strings.Contains(registerFullVersionList(s), `150.0.7871.182`) {
		return `"150.0.7871.182"`
	}
	if s != nil && strings.EqualFold(strings.TrimSpace(s.browser), "edge") {
		return `"143.0.0.0"`
	}
	return `"145.0.0.0"`
}

func (s *registerSession) registerAcceptLanguage() string {
	if s == nil || strings.TrimSpace(s.acceptLanguage) == "" {
		return "en,zh-CN;q=0.9,zh-TW;q=0.8,zh;q=0.7"
	}
	return s.acceptLanguage
}

func (s *registerSession) registerSecCHUAArch() string {
	if s == nil || strings.TrimSpace(s.secCHUAArch) == "" {
		return `"x86"`
	}
	return s.secCHUAArch
}

func (s *registerSession) registerSecCHUAPlatform() string {
	if s == nil || strings.TrimSpace(s.secCHUAPlatform) == "" {
		return `"macOS"`
	}
	return s.secCHUAPlatform
}

func (s *registerSession) registerSecCHUAPlatformVersion() string {
	if s == nil || strings.TrimSpace(s.secCHUAPlatformVer) == "" {
		return `"15.7.4"`
	}
	return s.secCHUAPlatformVer
}

func (s *registerSession) setRegisterClientHintHeaders(headers stdhttp.Header) {
	headers.Set("Sec-Ch-Ua", s.secCHUA)
	headers.Set("Sec-Ch-Ua-Mobile", "?0")
	headers.Set("Sec-Ch-Ua-Platform", s.registerSecCHUAPlatform())
}

func (s *registerSession) close() {
	if s != nil && s.client != nil {
		s.client.CloseIdleConnections()
	}
}

func (s *registerSession) do(ctx context.Context, method, rawURL string, headers map[string]string, body any) (*stdhttp.Response, error) {
	var rdr io.Reader
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
		rdr = bytes.NewReader(bodyBytes)
	}
	return s.doBytes(ctx, method, rawURL, headers, bodyBytes, rdr)
}

func (s *registerSession) doBytes(ctx context.Context, method, rawURL string, headers map[string]string, bodyBytes []byte, stdBody io.Reader) (*stdhttp.Response, error) {
	if s.curl != nil {
		resp, err := s.doCurl(ctx, method, rawURL, headers, bodyBytes)
		if err == nil {
			return resp, nil
		}
		if shouldDisableRegisterCurlImpersonate(err) {
			s.curl = nil
		}
	}
	if stdBody == nil && len(bodyBytes) > 0 {
		stdBody = bytes.NewReader(bodyBytes)
	}
	req, err := stdhttp.NewRequestWithContext(ctx, method, rawURL, stdBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept-Language", s.registerAcceptLanguage())
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Origin", registerAuthBase)
	req.Header.Set("Referer", registerAuthBase+"/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Sec-Ch-Ua", s.secCHUA)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", s.registerSecCHUAPlatform())
	for k, v := range headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}
	if len(bodyBytes) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	removeRegisterInternalHeaders(req.Header)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	finalRequest := resp.Request
	if finalRequest == nil {
		finalRequest = req
	}
	out := &stdhttp.Response{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Request:    finalRequest,
	}
	return out, nil
}

func (s *registerSession) doNoRedirect(ctx context.Context, method, rawURL string, headers map[string]string, body any) (*stdhttp.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := stdhttp.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept-Language", s.registerAcceptLanguage())
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Origin", registerAuthBase)
	req.Header.Set("Referer", registerAuthBase+"/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Sec-Ch-Ua", s.secCHUA)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", s.registerSecCHUAPlatform())
	for k, v := range headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *s.client
	client.CheckRedirect = func(req *stdhttp.Request, via []*stdhttp.Request) error {
		return stdhttp.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	finalRequest := resp.Request
	if finalRequest == nil {
		finalRequest = req
	}
	return &stdhttp.Response{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Request:    finalRequest,
	}, nil
}

func (s *registerSession) doCurl(ctx context.Context, method, rawURL string, headers map[string]string, bodyBytes []byte) (*stdhttp.Response, error) {
	var rdr io.Reader
	if len(bodyBytes) > 0 {
		rdr = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept-Language", s.registerAcceptLanguage())
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Origin", registerAuthBase)
	req.Header.Set("Referer", registerAuthBase+"/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Sec-Ch-Ua", s.secCHUA)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", s.registerSecCHUAPlatform())
	for k, v := range headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}
	if len(bodyBytes) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.client != nil && s.client.Jar != nil {
		if u, err := url.Parse(rawURL); err == nil {
			cookies := s.client.Jar.Cookies(u)
			if setter, ok := s.curl.(registerCurlCookieSetter); ok && len(cookies) > 0 {
				setter.SetCookies(rawURL, cookies)
			} else if len(cookies) > 0 && req.Header.Get("Cookie") == "" {
				parts := make([]string, 0, len(cookies))
				for _, cookie := range cookies {
					parts = append(parts, cookie.Name+"="+cookie.Value)
				}
				req.Header.Set("Cookie", strings.Join(parts, "; "))
			}
		}
	}
	resp, err := s.curl.Do(req)
	if err != nil {
		return nil, err
	}
	if s.client != nil && s.client.Jar != nil {
		if u, err := url.Parse(rawURL); err == nil {
			cookies := make([]*stdhttp.Cookie, 0, len(resp.Cookies()))
			for _, cookie := range resp.Cookies() {
				name := strings.TrimSpace(cookie.Name)
				value, ok := sanitizeCookieValue(cookie.Value)
				if name == "" || !ok {
					continue
				}
				cookies = append(cookies, &stdhttp.Cookie{Name: name, Value: value, Path: cookie.Path, Domain: cookie.Domain, Expires: cookie.Expires, MaxAge: cookie.MaxAge, Secure: cookie.Secure, HttpOnly: cookie.HttpOnly, SameSite: stdhttp.SameSite(cookie.SameSite)})
			}
			s.client.Jar.SetCookies(u, cookies)
		}
	}
	header := stdhttp.Header{}
	for key, values := range resp.Header {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	return &stdhttp.Response{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Header:     header,
		Body:       resp.Body,
		Request:    &stdhttp.Request{Method: method, URL: req.URL},
	}, nil
}

func sanitizeCookieValue(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	var b strings.Builder
	changed := false
	for _, r := range value {
		if r < 0x21 || r == '"' || r == ',' || r == ';' || r == '\\' || r == 0x7f {
			changed = true
			continue
		}
		b.WriteRune(r)
	}
	if !changed {
		return value, true
	}
	return b.String(), true
}

func isRegisterInternalHeader(key string) bool {
	return strings.EqualFold(strings.TrimSpace(key), "X-Curl-Follow-Redirect")
}

func removeRegisterInternalHeaders(headers stdhttp.Header) {
	for key := range headers {
		if isRegisterInternalHeader(key) {
			headers.Del(key)
		}
	}
}

func (s *registerSession) requestJSON(ctx context.Context, method, rawURL string, headers map[string]string, body any, out any, expected ...int) error {
	resp, err := s.do(ctx, method, rawURL, headers, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if len(expected) == 0 {
		expected = []int{200}
	}
	ok := false
	for _, code := range expected {
		if resp.StatusCode == code {
			ok = true
			break
		}
	}
	if !ok {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return registerHTTPError(method, rawURL, resp.StatusCode, string(b))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *registerSession) requestText(ctx context.Context, method, rawURL string, headers map[string]string, body any, expected ...int) (string, error) {
	text, _, err := s.requestTextResponse(ctx, method, rawURL, headers, body, expected...)
	return text, err
}

func (s *registerSession) requestTextResponse(ctx context.Context, method, rawURL string, headers map[string]string, body any, expected ...int) (string, *stdhttp.Response, error) {
	resp, err := s.do(ctx, method, rawURL, headers, body)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if len(expected) == 0 {
		expected = []int{200}
	}
	ok := false
	for _, code := range expected {
		if resp.StatusCode == code {
			ok = true
			break
		}
	}
	if !ok {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", resp, registerHTTPError(method, rawURL, resp.StatusCode, string(b))
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), resp, err
}

func (s *registerSession) requestTextStd(ctx context.Context, method, rawURL string, headers map[string]string, body any, expected ...int) (string, error) {
	text, _, err := s.requestTextStdResponse(ctx, method, rawURL, headers, body, expected...)
	return text, err
}

func (s *registerSession) requestTextStdResponse(ctx context.Context, method, rawURL string, headers map[string]string, body any, expected ...int) (string, *stdhttp.Response, error) {
	var saved upstreamHTTPDoer
	if s != nil {
		saved = s.curl
		s.curl = nil
		defer func() { s.curl = saved }()
	}
	resp, err := s.do(ctx, method, rawURL, headers, body)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if len(expected) == 0 {
		expected = []int{200}
	}
	ok := false
	for _, code := range expected {
		if resp.StatusCode == code {
			ok = true
			break
		}
	}
	if !ok {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", resp, registerHTTPError(method, rawURL, resp.StatusCode, string(b))
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), resp, err
}

func (s *registerSession) requestTextFollowRedirect(ctx context.Context, method, rawURL string, headers map[string]string, body any, maxRedirects int, expected ...int) (string, *stdhttp.Response, error) {
	if maxRedirects <= 0 {
		maxRedirects = 10
	}
	currentURL := rawURL
	currentMethod := method
	currentBody := body
	for redirects := 0; redirects <= maxRedirects; redirects++ {
		resp, err := s.do(ctx, currentMethod, currentURL, headers, currentBody)
		if err != nil {
			return "", nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
			location := strings.TrimSpace(resp.Header.Get("Location"))
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			nextURL, err := resolveRedirectURL(currentURL, location)
			if err != nil {
				return "", resp, err
			}
			currentURL = nextURL
			currentMethod = stdhttp.MethodGet
			currentBody = nil
			continue
		}
		defer resp.Body.Close()
		if len(expected) == 0 {
			expected = []int{200}
		}
		ok := false
		for _, code := range expected {
			if resp.StatusCode == code {
				ok = true
				break
			}
		}
		if !ok {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return "", resp, registerHTTPError(method, rawURL, resp.StatusCode, string(b))
		}
		b, err := io.ReadAll(resp.Body)
		return string(b), resp, err
	}
	return "", nil, fmt.Errorf("%s %s failed: too many redirects", method, rawURL)
}

func resolveRedirectURL(baseURL, location string) (string, error) {
	loc, err := url.Parse(strings.TrimSpace(location))
	if err != nil {
		return "", err
	}
	if loc.IsAbs() {
		return loc.String(), nil
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(loc).String(), nil
}

func registerHTTPError(method, rawURL string, status int, body string) error {
	body = strings.TrimSpace(body)
	lower := strings.ToLower(body)
	detail := body
	if len(detail) > 1200 {
		detail = detail[:1200]
	}
	if code, message, typ := registerOpenAIErrorFields(body); code != "" || message != "" {
		summary := []string{}
		if code != "" {
			summary = append(summary, "code="+code)
		}
		if typ != "" {
			summary = append(summary, "type="+typ)
		}
		if message != "" {
			summary = append(summary, "message="+message)
		}
		if code == "invalid_auth_step" {
			summary = append(summary, "hint=授权会话步骤与注册请求不匹配，请核对授权页最终路径和当前会话")
		}
		return fmt.Errorf("%s %s failed: status=%d %s body=%s", method, rawURL, status, strings.Join(summary, " "), detail)
	}
	switch {
	case status == 429:
		return fmt.Errorf("%s %s failed: status=%d", method, rawURL, status)
	case isRegisterCloudflareChallenge(status, lower):
		return fmt.Errorf("%s %s failed: 被 Cloudflare 拦截，请更换 IP/代理或配置 clearance 后重试: status=%d body=%s", method, rawURL, status, detail)
	case strings.Contains(body, "Failed to create account. Please try again."):
		return fmt.Errorf("%s %s failed: 创建账号失败，邮箱域名很可能因滥用被封禁，请更换邮箱域名: status=%d body=%s", method, rawURL, status, detail)
	default:
		return fmt.Errorf("%s %s failed: status=%d body=%s", method, rawURL, status, detail)
	}
}

func registerOpenAIErrorFields(body string) (string, string, string) {
	body = strings.TrimSpace(body)
	if body == "" || !json.Valid([]byte(body)) {
		return "", "", ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", "", ""
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		return "", "", ""
	}
	return strings.TrimSpace(strAny(errObj["code"], "")),
		strings.TrimSpace(strAny(errObj["message"], "")),
		strings.TrimSpace(strAny(errObj["type"], ""))
}

func shouldDisableRegisterCurlImpersonate(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "abort trap") ||
		strings.Contains(text, "libidn2") ||
		strings.Contains(text, "curl-impersonate failed")
}

func isRegisterCloudflareChallenge(status int, lowerBody string) bool {
	if status != 403 && status != 503 {
		return false
	}
	return strings.Contains(lowerBody, "<title>just a moment") ||
		strings.Contains(lowerBody, "<title>attention required! | cloudflare") ||
		strings.Contains(lowerBody, "cf-chl-") ||
		strings.Contains(lowerBody, "__cf_chl_") ||
		strings.Contains(lowerBody, "cf-browser-verification")
}

func (s *registerSession) createSentinelToken(ctx context.Context, deviceID, flow string) (string, error) {
	p := sentinelRequirementsToken(deviceID, s.userAgent)
	body := map[string]any{"p": p, "id": deviceID, "flow": flow}
	b, _ := json.Marshal(body)
	req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, "https://sentinel.openai.com/backend-api/sentinel/req", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Referer", "https://sentinel.openai.com/backend-api/sentinel/frame.html")
	req.Header.Set("Origin", "https://sentinel.openai.com")
	req.Header.Set("User-Agent", s.userAgent)
	s.setRegisterClientHintHeaders(req.Header)
	resp, err := s.doBytes(ctx, stdhttp.MethodPost, "https://sentinel.openai.com/backend-api/sentinel/req", map[string]string{
		"Content-Type":              "text/plain;charset=UTF-8",
		"Referer":                   "https://sentinel.openai.com/backend-api/sentinel/frame.html",
		"Origin":                    "https://sentinel.openai.com",
		"Sec-CH-UA":                 s.secCHUA,
		"Sec-CH-UA-Mobile":          "?0",
		"Sec-CH-UA-Platform":        s.registerSecCHUAPlatform(),
		"Sec-Fetch-Dest":            "empty",
		"Sec-Fetch-Mode":            "cors",
		"Sec-Fetch-Site":            "same-origin",
		"Sec-Fetch-Storage":         "none",
		"Sec-Fetch-User":            "?0",
		"Upgrade-Insecure-Requests": "0",
	}, b, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fallback, _ := json.Marshal(map[string]any{"p": sentinelRequirementsToken(deviceID, s.userAgent), "t": "", "c": "", "id": deviceID, "flow": flow})
		return string(fallback), nil
	}
	token := strings.TrimSpace(strAny(data["token"], ""))
	if resp.StatusCode != 200 || token == "" {
		return "", fmt.Errorf("sentinel_req_failed_%d", resp.StatusCode)
	}
	pow, _ := data["proofofwork"].(map[string]any)
	pValue := sentinelRequirementsToken(deviceID, s.userAgent)
	if boolAny(pow["required"], false) && strings.TrimSpace(strAny(pow["seed"], "")) != "" {
		pValue = sentinelProofToken(deviceID, s.userAgent, strAny(pow["seed"], ""), strAny(pow["difficulty"], "0"))
	}
	s.setAuthCookie("oai-sc", "0"+token)
	out, _ := json.Marshal(map[string]any{"p": pValue, "t": "", "c": token, "id": deviceID, "flow": flow})
	return string(out), nil
}

func (s *registerSession) setAuthCookie(name, value string) {
	if s == nil || s.client == nil || s.client.Jar == nil {
		return
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return
	}
	u, err := url.Parse(registerAuthBase)
	if err != nil {
		return
	}
	s.client.Jar.SetCookies(u, []*stdhttp.Cookie{{Name: name, Value: value, Path: "/", Secure: true}})
}

func generateRegisterPKCE() (string, string, error) {
	raw := make([]byte, 64)
	if _, err := crand.Read(raw); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomURLToken(n int) string {
	if n <= 0 {
		n = 32
	}
	raw := make([]byte, n)
	if _, err := crand.Read(raw); err != nil {
		return randID(n)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func registerRandomDecimalID() string {
	var raw [8]byte
	if _, err := crand.Read(raw[:]); err != nil {
		return strconv.FormatUint(uint64(rand.Int63()), 10)
	}
	return strconv.FormatUint(binary.BigEndian.Uint64(raw[:]), 10)
}

func registerTraceParent() string {
	var trace [16]byte
	var parent [8]byte
	if _, err := crand.Read(trace[:]); err != nil {
		return "00-" + strings.ReplaceAll(uuid4(), "-", "") + "-" + fmt.Sprintf("%016x", rand.Uint64()) + "-01"
	}
	if _, err := crand.Read(parent[:]); err != nil {
		return "00-" + base64.RawURLEncoding.EncodeToString(trace[:])[:32] + "-" + fmt.Sprintf("%016x", rand.Uint64()) + "-01"
	}
	return fmt.Sprintf("00-%x-%x-01", trace, parent)
}

func sentinelConfig(deviceID, ua string) []any {
	perf := 1000 + rand.Float64()*49000
	choices := func(values ...string) string { return values[rand.Intn(len(values))] }
	return []any{
		"1920x1080",
		time.Now().UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)",
		4294705152,
		rand.Float64(),
		ua,
		"https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js",
		nil,
		nil,
		"en-US",
		rand.Float64(),
		choices("vendorSub-undefined", "plugins-undefined", "mimeTypes-undefined", "hardwareConcurrency-undefined"),
		choices("location", "implementation", "URL", "documentURI", "compatMode"),
		choices("Object", "Function", "Array", "Number", "parseFloat", "undefined"),
		perf,
		deviceID,
		"",
		[]int{4, 8, 12, 16}[rand.Intn(4)],
		float64(time.Now().UnixMilli()) - perf,
	}
}

func sentinelB64(data any) string {
	b, _ := json.Marshal(data)
	return base64.StdEncoding.EncodeToString(b)
}

func sentinelRequirementsToken(deviceID, ua string) string {
	data := sentinelConfig(deviceID, ua)
	data[3] = 1
	data[9] = int(5 + rand.Float64()*45)
	return "gAAAAAC" + sentinelB64(data)
}

func sentinelFNV1a32(text string) string {
	var h uint32 = 2166136261
	for _, ch := range text {
		h ^= uint32(ch)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

func registerMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sentinelProofToken(deviceID, ua, seed, difficulty string) string {
	start := time.Now()
	data := sentinelConfig(deviceID, ua)
	if difficulty == "" {
		difficulty = "0"
	}
	for i := 0; i < 500000; i++ {
		data[3] = i
		data[9] = int(time.Since(start).Milliseconds())
		payload := sentinelB64(data)
		hash := sentinelFNV1a32(seed + payload)
		prefixLen := registerMinInt(len(hash), len(difficulty))
		if prefixLen == 0 || hash[:prefixLen] <= difficulty {
			return "gAAAAAB" + payload + "~S"
		}
	}
	return "gAAAAABwQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + sentinelB64(nil)
}

func (s *registerSession) platformAuthorize(ctx context.Context, deviceID, email string) (string, error) {
	return s.platformAuthorizeWithHint(ctx, deviceID, email, "signup")
}

func (s *registerSession) platformAuthorizeForRegistration(ctx context.Context, deviceID, email string) (string, bool, error) {
	verifier, err := s.platformAuthorizeRetry(ctx, deviceID, email, "signup", true)
	if err != nil {
		return "", false, err
	}
	if strings.HasPrefix(s.lastAuthorizeLanding, "signup path=/create-account/password") {
		return verifier, false, nil
	}
	if strings.HasPrefix(s.lastAuthorizeLanding, "verification path=") || strings.HasPrefix(s.lastAuthorizeLanding, "choice path=") || strings.HasPrefix(s.lastAuthorizeLanding, "login path=") {
		return verifier, true, nil
	}
	return "", false, fmt.Errorf("授权未进入明确的注册步骤: %s", s.lastAuthorizeLanding)
}

func (s *registerSession) platformAuthorizeEmailOTP(ctx context.Context, deviceID, email string) (string, error) {
	verifier, err := s.platformAuthorizeRetry(ctx, deviceID, email, "signup", true)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(s.lastAuthorizeLanding, "verification path=") {
		return "", fmt.Errorf("带邮箱授权未进入邮箱验证页: %s", s.lastAuthorizeLanding)
	}
	return verifier, nil
}

func (s *registerSession) platformAuthorizeWithHint(ctx context.Context, deviceID, email, screenHint string) (string, error) {
	includeLoginHint := true
	codeVerifier, err := s.platformAuthorizeRetry(ctx, deviceID, email, screenHint, includeLoginHint)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(strings.TrimSpace(screenHint), "signup") && strings.HasPrefix(s.lastAuthorizeLanding, "login path=") {
		return "", fmt.Errorf("授权进入登录页，当前邮箱或会话不在注册步骤: %s", s.lastAuthorizeLanding)
	}
	if !strings.EqualFold(strings.TrimSpace(screenHint), "signup") &&
		strings.Contains(strings.ToLower(s.lastAuthorizeLanding), "login") &&
		includeLoginHint {
		codeVerifier, err = s.platformAuthorizeRetry(ctx, deviceID, email, screenHint, false)
		if err != nil {
			return "", err
		}
	}
	return codeVerifier, nil
}

func (s *registerSession) platformAuthorizeRetry(ctx context.Context, deviceID, email, screenHint string, includeLoginHint bool) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		verifier, err := s.platformAuthorizeOnce(ctx, deviceID, email, screenHint, includeLoginHint)
		if err == nil || attempt == 1 || (!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
			return verifier, err
		}
		s.client.CloseIdleConnections()
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", errors.New("authorization retry exhausted")
}

func (s *registerSession) platformAuthorizeOnce(ctx context.Context, deviceID, email, screenHint string, includeLoginHint bool) (string, error) {
	codeVerifier, codeChallenge, err := generateRegisterPKCE()
	if err != nil {
		return "", err
	}
	if u, err := url.Parse(registerAuthBase); err == nil {
		s.client.Jar.SetCookies(u, []*stdhttp.Cookie{{Name: "oai-did", Value: deviceID, Path: "/"}})
	}
	params := url.Values{}
	params.Set("issuer", registerAuthBase)
	clientID := "app_2SKx67EdpoN0G6j64rFvigXD"
	redirectURI := registerPlatformURL + "/auth/callback"
	params.Set("client_id", clientID)
	params.Set("audience", "https://api.openai.com/v1")
	params.Set("redirect_uri", redirectURI)
	params.Set("device_id", deviceID)
	params.Set("screen_hint", firstNonEmpty(strings.TrimSpace(screenHint), "signup"))
	params.Set("max_age", "0")
	if includeLoginHint {
		params.Set("login_hint", email)
	}
	params.Set("scope", "openid profile email offline_access")
	params.Set("response_type", "code")
	params.Set("response_mode", "query")
	params.Set("state", randomURLToken(32))
	params.Set("nonce", randomURLToken(32))
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("auth0Client", registerAuth0Client)
	targetURL := registerAuthBase + "/api/accounts/authorize?" + params.Encode()
	s.lastAuthorizeDebug = registerFlowDebug{
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		ScreenHint:          params.Get("screen_hint"),
		Referer:             registerPlatformURL + "/",
		AccessFlowIDPresent: strings.TrimSpace(s.accessFlowID) != "",
	}
	text, resp, err := s.requestTextFollowRedirect(ctx, stdhttp.MethodGet, targetURL, map[string]string{
		"Accept":                      "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"DNT":                         "1",
		"Referer":                     registerPlatformURL + "/",
		"Sec-GPC":                     "1",
		"Sec-CH-UA-Arch":              s.registerSecCHUAArch(),
		"Sec-CH-UA-Bitness":           `"64"`,
		"Sec-CH-UA-Full-Version":      registerFullVersion(s),
		"Sec-CH-UA-Full-Version-List": registerFullVersionList(s),
		"Sec-CH-UA-Model":             `""`,
		"Sec-CH-UA-Platform-Version":  s.registerSecCHUAPlatformVersion(),
		"Sec-Fetch-Dest":              "document",
		"Sec-Fetch-Mode":              "navigate",
		"Sec-Fetch-Site":              "same-origin",
		"Sec-Fetch-User":              "?1",
		"Upgrade-Insecure-Requests":   "1",
		"X-Curl-Follow-Redirect":      "1",
	}, nil, 10, 200)
	if err != nil {
		return "", err
	}
	finalURL := ""
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	s.lastAuthorizeLanding = authorizeLandingSummary(finalURL, text)
	if !includeLoginHint {
		s.lastAuthorizeLanding += " no-login-hint"
	}
	return codeVerifier, nil
}

func authorizeLandingSummary(finalURL, body string) string {
	lowerURL := strings.ToLower(strings.TrimSpace(finalURL))
	lowerBody := strings.ToLower(body)
	landing := "unknown"
	switch {
	case strings.Contains(lowerURL, "/log-in-or-create-account"):
		landing = "choice"
	case strings.Contains(lowerURL, "/email-verification"):
		landing = "verification"
	case strings.Contains(lowerURL, "/create-account") || strings.Contains(lowerURL, "/signup"):
		landing = "signup"
	case strings.Contains(lowerURL, "/log-in") || strings.Contains(lowerURL, "/login"):
		landing = "login"
	case strings.Contains(lowerBody, "create-account/password") || strings.Contains(lowerBody, "create_account"):
		landing = "signup"
	case strings.Contains(lowerBody, "password_verification"):
		landing = "login"
	case strings.Contains(lowerBody, "cf-challenge") || strings.Contains(lowerBody, "cloudflare"):
		landing = "cloudflare"
	}
	if finalURL == "" {
		return landing
	}
	if parsed, err := url.Parse(finalURL); err == nil {
		return landing + " path=" + parsed.Path
	}
	return landing
}

func (s *registerSession) loginWithPassword(ctx context.Context, email, password, mfaSecret string, emailCode ...func(context.Context, time.Time) (string, error)) (map[string]any, error) {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return nil, errors.New("email and password are required")
	}
	deviceID := uuid4()
	codeVerifier, err := s.platformAuthorizeRetry(ctx, deviceID, email, "login_or_signup", true)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(s.lastAuthorizeLanding, "login path=/log-in/password") {
		return nil, fmt.Errorf("授权未进入密码登录步骤: %s", s.lastAuthorizeLanding)
	}
	sentinel, err := s.createSentinelToken(ctx, deviceID, "password_verify")
	if err != nil {
		return nil, err
	}
	body := map[string]any{"password": password}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/")
	headers["OpenAI-Sentinel-Token"] = sentinel
	resp, err := s.doBytes(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/password/verify", headers, b, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &payload)
	}
	if resp.StatusCode != 200 {
		return nil, registerHTTPError(stdhttp.MethodPost, registerAuthBase+"/api/accounts/password/verify", resp.StatusCode, string(raw))
	}
	if page, ok := payload["page"].(map[string]any); ok && strAny(page["type"], "") == "mfa_challenge" {
		if strings.TrimSpace(mfaSecret) == "" {
			return nil, errors.New("重新授权需要 2FA，账号未保存密钥")
		}
		payload, err = s.verifyPasswordLoginMFA(ctx, deviceID, payload, mfaSecret)
		if err != nil {
			return nil, err
		}
	}
	continueURL := strings.TrimSpace(strAny(payload["continue_url"], strAny(payload["url"], "")))
	code := parseContinueCode(continueURL)
	if code == "" {
		if page, ok := payload["page"].(map[string]any); ok && strings.TrimSpace(strAny(page["type"], "")) == "email_otp_verification" {
			if len(emailCode) == 0 || emailCode[0] == nil {
				return nil, errors.New("重新授权需要邮箱验证码，账号没有可用取码方式")
			}
			codeNotBefore := time.Now().UTC()
			if err := s.sendOTP(ctx, deviceID); err != nil {
				return nil, fmt.Errorf("发送邮箱验证码失败: %w", err)
			}
			otp, err := emailCode[0](ctx, codeNotBefore)
			if err != nil {
				return nil, fmt.Errorf("获取邮箱验证码失败: %w", err)
			}
			continueURL, err = s.validateOTPContinueURL(ctx, deviceID, otp)
			if err != nil {
				return nil, fmt.Errorf("校验邮箱验证码失败: %w", err)
			}
			code = parseContinueCode(continueURL)
			if code == "" {
				return nil, errors.New("邮箱验证码已校验，但未返回 oauth callback code")
			}
		} else {
			return nil, errors.New("password verify response missing oauth code")
		}
	}
	return s.exchangeTokens(ctx, codeVerifier, code)
}

func (s *registerSession) verifyPasswordLoginMFA(ctx context.Context, deviceID string, passwordResponse map[string]any, secret string) (map[string]any, error) {
	page, _ := passwordResponse["page"].(map[string]any)
	payload, _ := page["payload"].(map[string]any)
	factorID := strings.TrimSpace(strAny(payload["factor_id"], ""))
	factors, _ := payload["factors"].([]any)
	totpAvailable := false
	for _, raw := range factors {
		factor, _ := raw.(map[string]any)
		if strAny(factor["id"], "") == factorID && strAny(factor["factor_type"], "") == "totp" {
			totpAvailable = true
			break
		}
	}
	if factorID == "" || !totpAvailable {
		return nil, errors.New("2FA 登录未提供可用的 TOTP 验证方式")
	}
	headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/")
	if _, _, err := s.requestTextResponse(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/mfa/issue_challenge", headers, map[string]any{
		"id": factorID, "type": "totp", "force_fresh_challenge": false,
	}, stdhttp.StatusOK); err != nil {
		return nil, err
	}
	code, _, err := freshRegisterTOTP(ctx, secret)
	if err != nil {
		return nil, err
	}
	sentinel, err := s.createSentinelToken(ctx, deviceID, "verify_mfa")
	if err != nil {
		return nil, err
	}
	headers["OpenAI-Sentinel-Token"] = sentinel
	text, _, err := s.requestTextResponse(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/mfa/verify", headers, map[string]any{
		"id": factorID, "type": "totp", "code": code,
	}, stdhttp.StatusOK)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil, fmt.Errorf("解析 2FA 验证响应: %w", err)
	}
	if result["continue_url"] == nil {
		return nil, errors.New("2FA 验证响应缺少继续授权地址")
	}
	return result, nil
}

func (s *registerSession) registerUser(ctx context.Context, deviceID, email, password string) error {
	sentinel, err := s.createSentinelToken(ctx, deviceID, "username_password_create")
	if err != nil {
		return err
	}
	headers := s.registerJSONHeaders(deviceID, registerAuthBase+"/create-account/password")
	headers["OpenAI-Sentinel-Token"] = sentinel
	body := map[string]any{"username": email, "password": password}
	text, response, err := s.requestTextResponse(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/user/register", headers, body, 200, 302)
	if err != nil {
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "invalid_state") || strings.Contains(errText, "invalid_auth_step") {
			return fmt.Errorf("%w auth_debug={landing:%q,cookies:%v}", err, s.lastAuthorizeLanding, s.registerCookieNames())
		}
		return err
	}
	if err := registerUserResponseError(response, text); err != nil {
		return err
	}
	var resp map[string]any
	if strings.TrimSpace(text) != "" {
		_ = json.Unmarshal([]byte(text), &resp)
	}
	if msg := strings.TrimSpace(strAny(resp["message"], "")); msg != "" && strings.Contains(strings.ToLower(msg), "failed") {
		return errors.New(msg)
	}
	if page, ok := resp["page"].(map[string]any); ok {
		pageType := strings.TrimSpace(strAny(page["type"], ""))
		if pageType != "email_otp_send" {
			return fmt.Errorf("注册密码提交后返回意外步骤: %s", pageType)
		}
		s.registerOTPURL = strings.TrimSpace(strAny(resp["continue_url"], ""))
	} else if response.StatusCode == stdhttp.StatusOK {
		return errors.New("注册密码提交响应缺少下一步页面")
	}
	return nil
}

func (s *registerSession) preparePasswordSignup(ctx context.Context, deviceID string) error {
	sentinel, err := s.createSentinelToken(ctx, deviceID, "authorize_continue")
	if err != nil {
		return err
	}
	headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/email-verification")
	headers["OpenAI-Sentinel-Token"] = sentinel
	text, err := s.requestText(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/passwordless/send-otp", headers, nil, 200)
	if err != nil {
		return err
	}
	var response struct {
		Page struct {
			Type string `json:"type"`
		} `json:"page"`
	}
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return fmt.Errorf("邮箱验证码步骤响应解析失败: %w", err)
	}
	if response.Page.Type != "email_otp_verification" {
		return fmt.Errorf("邮箱验证码步骤不匹配: %s", response.Page.Type)
	}
	return nil
}

type registerAuthStepError struct {
	pageType string
}

func (e *registerAuthStepError) Error() string {
	return fmt.Sprintf("接续注册需要邮箱验证，当前步骤为 %q", e.pageType)
}

func (s *registerSession) authorizeContinue(ctx context.Context, deviceID, email string) error {
	sentinel, err := s.createSentinelToken(ctx, deviceID, "authorize_continue")
	if err != nil {
		return err
	}
	headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/log-in")
	headers["OpenAI-Sentinel-Token"] = sentinel
	text, err := s.requestText(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/authorize/continue", headers, map[string]any{
		"username": map[string]string{"kind": "email", "value": email},
	}, 200)
	if err != nil {
		return err
	}
	var response struct {
		Page struct {
			Type string `json:"type"`
		} `json:"page"`
	}
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return fmt.Errorf("接续注册响应解析失败: %w", err)
	}
	if response.Page.Type != "email_otp_verification" {
		return &registerAuthStepError{pageType: response.Page.Type}
	}
	return nil
}

func registerUserResponseError(resp *stdhttp.Response, body string) error {
	if resp == nil {
		return errors.New("注册密码提交未返回响应")
	}
	landingURL := strings.TrimSpace(resp.Header.Get("Location"))
	if landingURL == "" && resp.Request != nil && resp.Request.URL != nil {
		landingURL = resp.Request.URL.String()
	}
	landing := authorizeLandingSummary(landingURL, body)
	if strings.HasPrefix(landing, "login") {
		return fmt.Errorf("注册密码提交后进入登录页，未确认密码设置成功: %s", landing)
	}
	return nil
}

func (s *registerSession) registerCookieNames() []string {
	names := map[string]bool{}
	if s != nil && s.client != nil && s.client.Jar != nil {
		for _, rawURL := range []string{registerAuthBase + "/", registerPlatformURL + "/"} {
			u, err := url.Parse(rawURL)
			if err != nil {
				continue
			}
			for _, cookie := range s.client.Jar.Cookies(u) {
				name := strings.TrimSpace(cookie.Name)
				if name != "" {
					names[name] = true
				}
			}
		}
	}
	if s == nil || s.curl == nil {
		return sortedRegisterCookieNames(names)
	}
	return sortedRegisterCookieNames(names)
}

func sortedRegisterCookieNames(names map[string]bool) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s *registerSession) sendOTP(ctx context.Context, deviceID string) error {
	if s.registerOTPURL != "" {
		otpURL := s.registerOTPURL
		s.registerOTPURL = ""
		if otpURL != registerAuthBase+"/api/accounts/email-otp/send" {
			return fmt.Errorf("注册验证码发送地址不匹配")
		}
		_, err := s.requestText(ctx, stdhttp.MethodGet, otpURL, map[string]string{
			"Referer":       registerAuthBase + "/create-account/password",
			"Oai-Device-Id": deviceID,
		}, nil, 200, 302)
		return err
	}
	headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/email-verification")
	delete(headers, "Content-Type")
	if _, err := s.requestText(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/email-otp/resend", headers, nil, 200, 302); err == nil {
		return nil
	}
	_, err := s.requestText(ctx, stdhttp.MethodGet, registerAuthBase+"/api/accounts/email-otp/send", map[string]string{
		"Referer":       registerAuthBase + "/create-account/password",
		"Oai-Device-Id": deviceID,
	}, nil, 200, 302)
	return err
}

func (s *registerSession) registerJSONHeaders(deviceID, referer string) map[string]string {
	headers := s.registerCommonJSONHeaders(deviceID, referer)
	headers["X-OpenAI-Device-ID"] = deviceID
	headers["X-OpenAI-Target-Path"] = "/api/accounts/user/register"
	headers["X-OpenAI-Target-Route"] = "/api/accounts/user/register"
	headers["X-OpenAI-Sentinel-Flow"] = "username_password_create"
	return headers
}

func (s *registerSession) registerCommonJSONHeaders(deviceID, referer string) map[string]string {
	headers := map[string]string{
		"Accept":                      "application/json",
		"Accept-Language":             s.registerAcceptLanguage(),
		"Cache-Control":               "no-cache",
		"Content-Type":                "application/json",
		"DNT":                         "1",
		"Origin":                      registerAuthBase,
		"Referer":                     referer,
		"Oai-Device-Id":               deviceID,
		"Priority":                    "u=1, i",
		"Sec-GPC":                     "1",
		"Sec-CH-UA-Arch":              s.registerSecCHUAArch(),
		"Sec-CH-UA-Bitness":           `"64"`,
		"Sec-CH-UA-Full-Version":      registerFullVersion(s),
		"Sec-CH-UA-Full-Version-List": registerFullVersionList(s),
		"Sec-CH-UA-Model":             `""`,
		"Sec-CH-UA-Platform-Version":  s.registerSecCHUAPlatformVersion(),
		"Sec-Fetch-Dest":              "empty",
		"Sec-Fetch-Mode":              "cors",
		"Sec-Fetch-Site":              "same-origin",
		"traceparent":                 registerTraceParent(),
		"tracestate":                  "dd=s:1;o:rum",
		"x-datadog-origin":            "rum",
		"x-datadog-parent-id":         registerRandomDecimalID(),
		"x-datadog-sampling-priority": "1",
		"x-datadog-trace-id":          registerRandomDecimalID(),
	}
	if strings.TrimSpace(s.accessFlowID) != "" {
		headers["X-Access-Flow-Invocation-Id"] = strings.TrimSpace(s.accessFlowID)
	}
	if strings.TrimSpace(s.sentinelSOToken) != "" {
		headers["OpenAI-Sentinel-SO-Token"] = strings.TrimSpace(s.sentinelSOToken)
	}
	return headers
}

func (s *registerSession) validateOTP(ctx context.Context, deviceID, code string) (string, error) {
	return s.validateOTPContinueURL(ctx, deviceID, code)
}

func (s *registerSession) validateOTPContinueURL(ctx context.Context, deviceID, code string) (string, error) {
	rawURL := registerAuthBase + "/api/accounts/email-otp/validate"
	headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/email-verification")
	sentinelPresent := false
	if sentinel, sentinelErr := s.createSentinelToken(ctx, deviceID, "email_otp_validate"); sentinelErr == nil {
		headers["OpenAI-Sentinel-Token"] = sentinel
		sentinelPresent = strings.TrimSpace(sentinel) != ""
	}
	s.lastValidateOTPDebug = registerFlowDebug{
		Referer:              registerAuthBase + "/email-verification",
		Flow:                 "email_otp_validate",
		SOTokenPresent:       strings.TrimSpace(s.sentinelSOToken) != "",
		SentinelTokenPresent: sentinelPresent,
		AccessFlowIDPresent:  strings.TrimSpace(s.accessFlowID) != "",
	}
	text, resp, err := s.requestTextResponse(ctx, stdhttp.MethodPost, rawURL, headers, map[string]any{"code": code}, 200, 302)
	if err != nil {
		headers = s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/email-verification")
		s.lastValidateOTPDebug.SentinelTokenPresent = false
		text, resp, err = s.requestTextResponse(ctx, stdhttp.MethodPost, rawURL, headers, map[string]any{"code": code}, 200, 302)
		if err != nil {
			return "", err
		}
	}
	return continueURLFromResponse(resp, text)
}

func (s *registerSession) visitAuthPage(ctx context.Context, rawURL, referer string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		rawURL = registerAuthBase + "/about-you"
	}
	if strings.HasPrefix(rawURL, "/") {
		rawURL = registerAuthBase + rawURL
	}
	if referer == "" {
		referer = registerAuthBase + "/email-verification"
	}
	_, _, err := s.requestTextResponse(ctx, stdhttp.MethodGet, rawURL, map[string]string{
		"Accept":                      "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"DNT":                         "1",
		"Referer":                     referer,
		"Sec-GPC":                     "1",
		"Sec-CH-UA-Arch":              s.registerSecCHUAArch(),
		"Sec-CH-UA-Bitness":           `"64"`,
		"Sec-CH-UA-Full-Version-List": registerFullVersionList(s),
		"Sec-CH-UA-Model":             `""`,
		"Sec-CH-UA-Platform-Version":  s.registerSecCHUAPlatformVersion(),
		"Sec-Fetch-Dest":              "document",
		"Sec-Fetch-Mode":              "navigate",
		"Sec-Fetch-Site":              "same-origin",
		"Sec-Fetch-User":              "?1",
		"Upgrade-Insecure-Requests":   "1",
		"X-Curl-Follow-Redirect":      "1",
	}, nil, 200)
	return err
}

func (s *registerSession) createAccount(ctx context.Context, deviceID string, profile registerProfileConfig) (string, error) {
	body := map[string]any{"name": profile.name(), "birthdate": profile.birthdate()}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		sentinel, err := s.createSentinelToken(ctx, deviceID, "oauth_create_account")
		if err != nil {
			return "", err
		}
		headers := s.registerCommonJSONHeaders(deviceID, registerAuthBase+"/about-you")
		headers["OpenAI-Sentinel-Token"] = sentinel
		s.lastCreateDebug = registerFlowDebug{
			Referer:              registerAuthBase + "/about-you",
			Flow:                 "oauth_create_account",
			SOTokenPresent:       strings.TrimSpace(s.sentinelSOToken) != "",
			SentinelTokenPresent: strings.TrimSpace(sentinel) != "",
			AccessFlowIDPresent:  strings.TrimSpace(s.accessFlowID) != "",
		}
		text, resp, err := s.requestTextResponse(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/create_account", headers, body, 200, 302)
		if err != nil {
			lastErr = err
			if attempt < 3 && registerShouldRetryCreateAccount(err) {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return "", err
		}
		continueURL, err := continueURLFromResponse(resp, text)
		if err != nil {
			return "", err
		}
		if continueURL == "" {
			lastErr = fmt.Errorf(
				"create account response missing continue_url status=%d location=%q headers=%v body=%s",
				responseStatusCode(resp),
				responseLocation(resp),
				responseHeaderNames(resp),
				truncateText(strings.TrimSpace(text), 800),
			)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return "", lastErr
		}
		return continueURL, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("create account failed")
}

func responseStatusCode(resp *stdhttp.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func responseLocation(resp *stdhttp.Response) string {
	if resp == nil {
		return ""
	}
	return truncateText(resp.Header.Get("Location"), 240)
}

func responseHeaderNames(resp *stdhttp.Response) []string {
	if resp == nil {
		return nil
	}
	names := make([]string, 0, len(resp.Header))
	for name := range resp.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func registerShouldRetryCreateAccount(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "status=500") ||
		strings.Contains(text, "status=502") ||
		strings.Contains(text, "status=503") ||
		strings.Contains(text, "status=504")
}

func continueURLFromCreateAccountResponse(resp *stdhttp.Response) (string, error) {
	return continueURLFromResponse(resp, "")
}

func continueURLFromResponse(resp *stdhttp.Response, bodyText string) (string, error) {
	if resp == nil {
		return "", errors.New("create account response is nil")
	}
	bodyBytes := bytes.TrimSpace([]byte(bodyText))
	if len(bodyBytes) == 0 && resp.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", err
		}
		bodyBytes = bytes.TrimSpace(bodyBytes)
	}
	if len(bodyBytes) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(bodyBytes, &payload); err == nil {
			if continueURL := strings.TrimSpace(strAny(payload["continue_url"], strAny(payload["url"], ""))); continueURL != "" {
				if code := parseContinueCode(continueURL); code != "" {
					return continueURL, nil
				}
			}
		}
	}
	continueURL := strings.TrimSpace(resp.Header.Get("Location"))
	if err := registerAuthErrorFromLocation(continueURL); err != nil {
		return "", err
	}
	if continueURL != "" && parseContinueCode(continueURL) != "" {
		return continueURL, nil
	}
	if resp.Request != nil && resp.Request.URL != nil {
		continueURL = strings.TrimSpace(resp.Request.URL.String())
		if err := registerAuthErrorFromLocation(continueURL); err != nil {
			return "", err
		}
		if continueURL != "" && parseContinueCode(continueURL) != "" {
			return continueURL, nil
		}
	}
	return "", nil
}

func registerAuthErrorFromLocation(location string) error {
	location = strings.TrimSpace(location)
	if location == "" {
		return nil
	}
	u, err := url.Parse(location)
	if err != nil {
		return nil
	}
	payloadText := strings.TrimSpace(u.Query().Get("payload"))
	if payloadText == "" {
		return nil
	}
	var decoded []byte
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		decoded, err = enc.DecodeString(payloadText)
		if err == nil {
			break
		}
	}
	if err != nil || len(decoded) == 0 {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil
	}
	code := strings.TrimSpace(firstNonEmpty(strAny(payload["errorCode"], ""), strAny(payload["code"], "")))
	message := strings.TrimSpace(firstNonEmpty(strAny(payload["error"], ""), strAny(payload["message"], "")))
	kind := strings.TrimSpace(strAny(payload["kind"], ""))
	requestID := strings.TrimSpace(strAny(payload["requestId"], ""))
	parts := []string{}
	if code != "" {
		parts = append(parts, "code="+code)
	}
	if kind != "" {
		parts = append(parts, "kind="+kind)
	}
	if message != "" {
		parts = append(parts, "message="+message)
	}
	if requestID != "" {
		parts = append(parts, "request_id="+requestID)
	}
	switch code {
	case "registration_disallowed":
		parts = append(parts, "hint=OpenAI 拒绝创建该账号，通常是邮箱别名/域名/IP/风控环境被判定不可注册，建议换新别名或代理")
	case "invalid_auth_step":
		parts = append(parts, "hint=授权会话步骤与注册请求不匹配，请核对授权页最终路径和当前会话")
	case "invalid_state":
		parts = append(parts, "hint=登录会话已失效，需要重新开始注册流程")
	}
	if len(parts) == 0 {
		parts = append(parts, "payload="+truncateText(string(decoded), 300))
	}
	return fmt.Errorf("create account failed: %s", strings.Join(parts, " "))
}

func (s *registerSession) exchangeTokens(ctx context.Context, codeVerifier, code string) (map[string]any, error) {
	if strings.TrimSpace(codeVerifier) == "" {
		return nil, errors.New("oauth code_verifier is empty")
	}
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("oauth callback code is empty")
	}
	body := map[string]any{
		"client_id":     "app_2SKx67EdpoN0G6j64rFvigXD",
		"code_verifier": codeVerifier,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  registerPlatformURL + "/auth/callback",
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := s.doBytes(ctx, stdhttp.MethodPost, registerAuthBase+"/api/accounts/oauth/token", map[string]string{
		"Content-Type": "application/json",
		"Accept":       "*/*",
		"Origin":       registerPlatformURL,
		"Referer":      registerPlatformURL + "/",
		"Auth0-Client": registerAuth0Client,
	}, b, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, registerHTTPError(stdhttp.MethodPost, registerAuthBase+"/api/accounts/oauth/token", resp.StatusCode, string(b))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseContinueCode(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("code"))
}

func randomBirthdate() string {
	now := time.Now().UTC()
	start := time.Date(now.Year()-30, now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// Leave one day of margin at the lower age boundary so leap-year day-of-year
	// comparisons cannot classify an exact birthday as one year too young.
	end := time.Date(now.Year()-18, now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	days := int(end.Sub(start).Hours() / 24)
	if days <= 0 {
		return end.Format("2006-01-02")
	}
	return start.AddDate(0, 0, rand.Intn(days+1)).Format("2006-01-02")
}

type registerProfileConfig struct {
	Name      string
	Birthdate string
}

func (p registerProfileConfig) name() string {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name
	}
	first, last := randomName()
	return first + " " + last
}

func (p registerProfileConfig) birthdate() string {
	if birthdate := strings.TrimSpace(p.Birthdate); birthdate != "" {
		return birthdate
	}
	return randomBirthdate()
}

func randomName() (string, string) {
	firsts := []string{"James", "Robert", "John", "Michael", "David", "Mary", "Emma", "Olivia"}
	lasts := []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller"}
	return firsts[rand.Intn(len(firsts))], lasts[rand.Intn(len(lasts))]
}

func randomMailboxName() string {
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	b := strings.Builder{}
	for i := 0; i < 8; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type registerTaskResult struct {
	OK         bool
	Message    string
	Stop       bool
	StopReason string
	Counted    bool
}

type registerCapacityExhaustedError struct {
	message string
}

func (e registerCapacityExhaustedError) Error() string { return e.message }

type registerPasswordStepUnavailableError struct {
	message string
}

func (e registerPasswordStepUnavailableError) Error() string { return e.message }

func (s *registerService) probePasswordSignup(ctx context.Context, cfg RegisterConfig) error {
	route, err := s.resolveRegisterProxy(cfg.Proxy)
	if err != nil {
		return err
	}
	provider, err := s.createProviderWithProxy(cfg.Mail, route.URL)
	if err != nil {
		return err
	}
	defer provider.Close()
	probeEmail := "registration-probe-" + randomToken(12) + "@example.com"
	_, resume, err := provider.Session().platformAuthorizeForRegistration(ctx, uuid4(), probeEmail)
	if err != nil {
		return fmt.Errorf("密码注册步骤预检失败: %w", err)
	}
	landing := ""
	if rs, ok := provider.Session().(*registerSession); ok {
		landing = rs.lastAuthorizeLanding
	}
	if resume && !strings.HasPrefix(landing, "verification path=") {
		return registerPasswordStepUnavailableError{message: "授权未进入支持密码注册的步骤；未领取邮箱或创建账号"}
	}
	return nil
}

func (s *registerService) runRepairAbnormalJob(ctx context.Context) {
	var candidates []Account
	if s.server != nil && s.server.store != nil {
		for _, account := range s.server.store.LoadAccounts() {
			if account.Status == "异常" {
				candidates = append(candidates, account)
			}
		}
	}
	s.appendLog(fmt.Sprintf("待修复异常账号 %d 个", len(candidates)), "yellow")
	success, fail := 0, 0
	for _, account := range candidates {
		if ctx.Err() != nil {
			break
		}
		s.mu.Lock()
		s.cfg.Stats.Running = 1
		s.cfg.Stats.UpdatedAt = nowISO()
		_ = s.saveLocked()
		s.mu.Unlock()
		relogined, _, errs, _ := s.server.reLoginAccounts(ctx, []string{account.AccessToken})
		if relogined > 0 {
			success++
			s.appendLog(fmt.Sprintf("%s 异常账号修复成功", ptrString(account.Email)), "green")
		} else {
			fail++
			reason := "账号缺少可用的重新授权方式"
			if len(errs) > 0 {
				reason = strAny(errs[0]["error"], reason)
			}
			message := fmt.Sprintf("%s 异常账号修复失败: %s", ptrString(account.Email), reason)
			s.appendLog(message, "red")
			s.recordFailure(s.snapshot().Stats.JobID, success+fail, errors.New(message))
		}
		s.mu.Lock()
		s.cfg.Stats.Success = success
		s.cfg.Stats.Fail = fail
		s.cfg.Stats.Done = success + fail
		s.cfg.Stats.Running = 0
		s.cfg.Stats.UpdatedAt = nowISO()
		_ = s.saveLocked()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.cfg.Enabled = false
	s.cfg.Stats.Success = success
	s.cfg.Stats.Fail = fail
	s.cfg.Stats.Done = success + fail
	s.cfg.Stats.Running = 0
	s.cfg.Stats.FinishedAt = nowISO()
	s.cfg.Stats.UpdatedAt = nowISO()
	s.logs = append(s.logs, registerLogItem{Time: nowISO(), Text: fmt.Sprintf("异常账号修复结束，成功%d，失败%d", success, fail), Level: "yellow"})
	s.cancel = nil
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *registerService) runRegisterJob(ctx context.Context) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	threads := maxInt(cfg.Threads, 1)
	submitted := 0
	done := 0
	success := 0
	fail := 0
	results := make(chan registerTaskResult, threads)
	var wg sync.WaitGroup
	if s.passwordStepProbe != nil {
		if err := s.passwordStepProbe(ctx, cfg); err != nil {
			s.appendLog("注册任务停止："+err.Error(), "red")
			goto finish
		}
	}
	for {
		select {
		case <-ctx.Done():
			goto finish
		default:
		}
		snapshot := s.snapshot()
		if !snapshot.Enabled {
			break
		}
		if s.registerTargetReached(snapshot, submitted) {
			break
		}
		running := submitted - done
		for running < threads && !s.registerTargetReached(snapshot, submitted) && snapshot.Enabled && ctx.Err() == nil {
			submitted++
			running++
			wg.Add(1)
			index := submitted
			go func(jobIndex int, snapshot RegisterConfig) {
				defer wg.Done()
				ok, err := s.runRegisterTask(ctx, snapshot, jobIndex)
				result := registerTaskResult{OK: ok, Counted: true}
				if err != nil {
					result.Message = fmt.Sprintf("[任务%d] %s", jobIndex, err.Error())
					var passwordErr registerPasswordStepUnavailableError
					result.Stop = shouldStopForRegisterCapacityExhausted(err) || errors.As(err, &passwordErr)
					if passwordErr.message != "" {
						result.StopReason = "上游未提供设置密码步骤"
					} else if result.Stop {
						result.StopReason = "邮箱 provider 暂无可用资源"
					}
					if result.Stop {
						result.Counted = false
					} else {
						s.recordFailure(snapshot.Stats.JobID, jobIndex, err)
					}
				}
				select {
				case results <- result:
				case <-ctx.Done():
				}
			}(index, snapshot)
		}
		s.updateRegisterStats(submitted, done, success, fail, running)
		if running == 0 {
			time.Sleep(time.Duration(maxInt(snapshot.CheckInterval, 5)) * time.Second)
			continue
		}
		select {
		case result := <-results:
			running--
			if result.Counted {
				done++
				if result.OK {
					success++
				} else {
					fail++
				}
			}
			if result.Message != "" {
				level := "red"
				if result.Stop {
					level = "yellow"
				}
				s.appendLog(result.Message, level)
			}
			if result.Stop {
				s.appendLog("注册任务停止："+result.StopReason, "yellow")
				goto finish
			}
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			goto finish
		}
	}
finish:
	wg.Wait()
	for {
		select {
		case result := <-results:
			if result.Counted {
				done++
				if result.OK {
					success++
				} else {
					fail++
				}
			}
			if result.Message != "" {
				level := "red"
				if result.Stop {
					level = "yellow"
				}
				s.appendLog(result.Message, level)
			}
		default:
			goto drained
		}
	}
drained:
	s.mu.Lock()
	s.cfg.Enabled = false
	s.cfg.Stats.Running = 0
	s.cfg.Stats.Done = done
	s.cfg.Stats.Success = success
	s.cfg.Stats.Fail = fail
	s.cfg.Stats.FinishedAt = nowISO()
	s.cfg.Stats.UpdatedAt = nowISO()
	s.logs = append(s.logs, registerLogItem{Time: nowISO(), Text: fmt.Sprintf("注册任务结束，成功%d，失败%d", success, fail), Level: "yellow"})
	s.cancel = nil
	_ = s.saveLocked()
	s.mu.Unlock()
}

func isRegisterCapacityExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	var capacityErr registerCapacityExhaustedError
	if errors.As(err, &capacityErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "暂无可用别名") ||
		strings.Contains(text, "别名池为空") ||
		strings.Contains(text, "邮箱池暂无可用") ||
		strings.Contains(text, "mailbox pool") ||
		strings.Contains(text, "alias pool")
}

func shouldStopForRegisterCapacityExhausted(err error) bool {
	var capacityErr registerCapacityExhaustedError
	return errors.As(err, &capacityErr)
}

func (s *registerService) updateRegisterStats(submitted, done, success, fail, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Stats.Done = done
	s.cfg.Stats.Success = success
	s.cfg.Stats.Fail = fail
	s.cfg.Stats.Running = running
	s.cfg.Stats.Threads = maxInt(s.cfg.Threads, 1)
	s.cfg.Stats.CurrentQuota = s.currentRegisterQuota()
	s.cfg.Stats.CurrentAvailable = s.currentRegisterAvailable()
	s.cfg.Stats.UpdatedAt = nowISO()
	if started := strings.TrimSpace(s.cfg.Stats.StartedAt); started != "" {
		if st, err := time.Parse(time.RFC3339Nano, started); err == nil {
			elapsed := time.Since(st).Seconds()
			s.cfg.Stats.ElapsedSeconds = round1(elapsed)
			if success > 0 {
				s.cfg.Stats.AvgSeconds = round1(elapsed / float64(success))
			}
			if success+fail > 0 {
				s.cfg.Stats.SuccessRate = round1(float64(success) * 100 / float64(success+fail))
			}
		}
	}
	_ = s.saveLocked()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func (s *registerService) registerTargetReached(cfg RegisterConfig, submitted int) bool {
	switch strings.TrimSpace(cfg.Mode) {
	case "quota":
		return s.currentRegisterQuota() >= cfg.TargetQuota
	case "available":
		return s.currentRegisterAvailable() >= cfg.TargetAvailable
	default:
		return submitted >= cfg.Total
	}
}

func (s *registerService) currentRegisterQuota() int {
	accounts := s.loadCurrentAccounts()
	sum := 0
	for _, a := range accounts {
		if a.Status == "正常" {
			sum += a.Quota
		}
	}
	return sum
}

func (s *registerService) currentRegisterAvailable() int {
	accounts := s.loadCurrentAccounts()
	count := 0
	for _, a := range accounts {
		if a.Status == "正常" {
			count++
		}
	}
	return count
}

func (s *registerService) loadCurrentAccounts() []Account {
	if s == nil || s.server == nil || s.server.store == nil {
		return nil
	}
	return s.server.store.LoadAccounts()
}

func (s *registerService) importRegisterAccount(rec map[string]any) error {
	if s == nil || s.server == nil || s.server.store == nil {
		return errors.New("account store unavailable")
	}
	token := strings.TrimSpace(strAny(rec["access_token"], strAny(rec["accessToken"], "")))
	if token == "" {
		return errors.New("access token missing")
	}
	source := strings.TrimSpace(strAny(rec["source_type"], "web"))
	if source == "" {
		source = "web"
	}
	next := accountFromRecord(token, source, rec)
	return s.server.store.UpdateAccounts(func(accounts []Account) []Account {
		for i, account := range accounts {
			if account.AccessToken == token {
				mergeAccount(&account, next)
				accounts[i] = account
				return accounts
			}
		}
		return append(accounts, next)
	})
}

func (s *registerService) nextProviderEntry(entries []map[string]any) map[string]any {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.providerIndex % len(entries)
	s.providerIndex = (s.providerIndex + 1) % len(entries)
	entry := map[string]any{}
	for k, v := range entries[idx] {
		entry[k] = v
	}
	if entry["type"] == nil {
		entry["type"] = "tempmail_lol"
	}
	return entry
}

func (s *registerService) enabledRegisterProviderCount(mail RegisterMailConfig) int {
	count := 0
	for _, raw := range mail.Providers {
		if raw == nil {
			continue
		}
		if enabled, ok := raw["enable"]; ok && !boolAny(enabled, true) {
			continue
		}
		count++
	}
	return count
}

func (s *registerService) runRegisterTask(ctx context.Context, cfg RegisterConfig, index int) (bool, error) {
	s.appendLog(fmt.Sprintf("[任务%d] 任务启动", index), "yellow")
	route, err := s.resolveRegisterProxy(cfg.Proxy)
	if err != nil {
		return false, fmt.Errorf("选择注册代理失败: %w", err)
	}
	if route.NodeID != "" {
		s.appendLog(fmt.Sprintf("[任务%d] 注册代理节点[%s]", index, route.NodeID), "yellow")
	}
	var provider registerProvider
	var mailbox *registerMailbox
	var createErrors []string
	createAttempts := 0
	capacityExhaustedAttempts := 0
	for attempt := 0; attempt < maxInt(s.enabledRegisterProviderCount(cfg.Mail), 1); attempt++ {
		provider, err = s.createProviderWithProxy(cfg.Mail, route.URL)
		if err != nil {
			createErrors = append(createErrors, err.Error())
			createAttempts++
			if isRegisterCapacityExhaustedError(err) {
				capacityExhaustedAttempts++
			}
			continue
		}
		mailbox, err = provider.CreateMailbox("")
		if err == nil {
			break
		}
		createErrors = append(createErrors, fmt.Sprintf("%s: %s", provider.Name(), err.Error()))
		createAttempts++
		if isRegisterCapacityExhaustedError(err) {
			capacityExhaustedAttempts++
		}
		provider.Close()
		provider = nil
	}
	if provider == nil || mailbox == nil {
		if len(createErrors) > 0 {
			message := strings.Join(createErrors, "; ")
			if createAttempts > 0 && capacityExhaustedAttempts == createAttempts {
				return false, registerCapacityExhaustedError{message: message}
			}
			return false, errors.New(message)
		}
		return false, errors.New("创建邮箱失败")
	}
	email := strings.TrimSpace(mailbox.Address)
	defer provider.Close()
	mailbox.CodeNotBefore = time.Now().UTC()
	if email == "" {
		return false, errors.New("邮箱服务未返回 address")
	}
	label := strings.TrimSpace(firstNonEmpty(mailbox.Label, mailbox.ProviderRef, mailbox.Provider))
	if label != "" {
		s.appendLog(fmt.Sprintf("[任务%d] 邮箱创建完成[%s]: %s", index, label, email), "yellow")
	} else {
		s.appendLog(fmt.Sprintf("[任务%d] 邮箱创建完成: %s", index, email), "yellow")
	}
	fail := func(err error) (bool, error) {
		if isRegisterContextCanceled(err) {
			return false, err
		}
		reason := err.Error()
		if email != "" {
			reason = fmt.Sprintf("%s: %s", email, reason)
		}
		provider.MarkFailed(mailbox, reason)
		return false, errors.New(reason)
	}
	password := cfg.FixedPassword
	if strings.TrimSpace(password) == "" {
		password = randomPassword()
	}
	deviceID := uuid4()
	s.appendLog(fmt.Sprintf("[任务%d] 开始授权注册页", index), "yellow")
	codeVerifier, resumeRegistration, err := provider.Session().platformAuthorizeForRegistration(ctx, deviceID, email)
	if err != nil {
		return fail(err)
	}
	landing := ""
	if rs, ok := provider.Session().(*registerSession); ok {
		landing = strings.TrimSpace(rs.lastAuthorizeLanding)
	}
	if resumeRegistration && !strings.HasPrefix(landing, "verification path=") {
		message := fmt.Sprintf("%s: 授权未进入设置密码页，账号未创建；邮箱已领取，需保留供后续处理", email)
		provider.MarkFailed(mailbox, message)
		return false, registerPasswordStepUnavailableError{message: message}
	}
	if landing != "" {
		s.appendLog(fmt.Sprintf("[任务%d] 授权注册页完成[%s]", index, landing), "yellow")
	} else {
		s.appendLog(fmt.Sprintf("[任务%d] 授权注册页完成", index), "yellow")
	}
	if rs, ok := provider.Session().(*registerSession); ok {
		s.appendLog(fmt.Sprintf("[任务%d] 授权注册页调试[%s]", index, rs.lastAuthorizeDebug.String()), "yellow")
	}
	if resumeRegistration {
		s.appendLog(fmt.Sprintf("[任务%d] 开始建立邮箱验证码步骤", index), "yellow")
		if err := provider.Session().preparePasswordSignup(ctx, deviceID); err != nil {
			return fail(err)
		}
	}
	s.appendLog(fmt.Sprintf("[任务%d] 开始提交注册密码", index), "yellow")
	if err := provider.Session().registerUser(ctx, deviceID, email, password); err != nil {
		return fail(err)
	}
	s.appendLog(fmt.Sprintf("[任务%d] 提交注册密码完成", index), "yellow")
	s.appendLog(fmt.Sprintf("[任务%d] 开始发送验证码", index), "yellow")
	if err := provider.Session().sendOTP(ctx, deviceID); err != nil {
		return fail(err)
	}
	s.appendLog(fmt.Sprintf("[任务%d] 发送验证码完成", index), "yellow")
	validated := false
	aboutYouURL := ""
	triedCodes := map[string]bool{}
	for attempt := 1; attempt <= 3; attempt++ {
		s.appendLog(fmt.Sprintf("[任务%d] 开始等待注册验证码", index), "yellow")
		code, err := provider.WaitForCode(ctx, mailbox)
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(code) == "" {
			return fail(errors.New("等待注册验证码超时"))
		}
		code = strings.TrimSpace(code)
		if triedCodes[code] {
			if attempt < 3 {
				s.appendLog(fmt.Sprintf("[任务%d] 收到重复验证码，继续等待下一封验证码", index), "yellow")
				time.Sleep(time.Duration(maxInt(cfg.Mail.WaitInterval, 2)) * time.Second)
				continue
			}
			return fail(errors.New("重复收到相同验证码"))
		}
		triedCodes[code] = true
		codeRecipient := ""
		if mailbox.Extra != nil {
			codeRecipient = strings.TrimSpace(strAny(mailbox.Extra["imap_message_recipient"], ""))
		}
		if codeRecipient != "" {
			s.appendLog(fmt.Sprintf("[任务%d] 收到注册验证码 for %s matched %s", index, email, codeRecipient), "yellow")
		} else {
			s.appendLog(fmt.Sprintf("[任务%d] 收到注册验证码 for %s", index, email), "yellow")
		}
		s.appendLog(fmt.Sprintf("[任务%d] 开始校验验证码", index), "yellow")
		nextURL, err := provider.Session().validateOTP(ctx, deviceID, code)
		if err != nil {
			if isWrongEmailOTPCode(err) && attempt < 3 {
				s.appendLog(fmt.Sprintf("[任务%d] 验证码错误，继续等待下一封验证码", index), "yellow")
				continue
			}
			return fail(err)
		}
		if strings.TrimSpace(nextURL) != "" {
			aboutYouURL = strings.TrimSpace(nextURL)
			s.appendLog(fmt.Sprintf("[任务%d] 验证码校验返回下一步[%s]", index, truncateText(nextURL, 160)), "yellow")
		}
		validated = true
		break
	}
	if !validated {
		return fail(errors.New("验证码校验失败"))
	}
	s.appendLog(fmt.Sprintf("[任务%d] 验证码校验完成", index), "yellow")
	if rs, ok := provider.Session().(*registerSession); ok {
		s.appendLog(fmt.Sprintf("[任务%d] 验证码校验调试[%s]", index, rs.lastValidateOTPDebug.String()), "yellow")
	}
	s.appendLog(fmt.Sprintf("[任务%d] 开始进入账号资料页", index), "yellow")
	if err := provider.Session().visitAuthPage(ctx, aboutYouURL, registerAuthBase+"/email-verification"); err != nil {
		return fail(err)
	}
	s.appendLog(fmt.Sprintf("[任务%d] 进入账号资料页完成", index), "yellow")
	s.appendLog(fmt.Sprintf("[任务%d] 开始创建账号资料", index), "yellow")
	if rs, ok := provider.Session().(*registerSession); ok {
		rs.lastCreateDebug = registerFlowDebug{
			Referer:             registerAuthBase + "/about-you",
			Flow:                "oauth_create_account",
			SOTokenPresent:      strings.TrimSpace(rs.sentinelSOToken) != "",
			AccessFlowIDPresent: strings.TrimSpace(rs.accessFlowID) != "",
		}
		s.appendLog(fmt.Sprintf("[任务%d] 创建账号资料调试[%s]", index, rs.lastCreateDebug.String()), "yellow")
	}
	continueURL, err := provider.Session().createAccount(ctx, deviceID, registerProfileConfig{
		Name:      cfg.ProfileName,
		Birthdate: cfg.ProfileBirthdate,
	})
	if err != nil {
		if rs, ok := provider.Session().(*registerSession); ok {
			s.appendLog(fmt.Sprintf("[任务%d] 创建账号资料失败调试[%s]", index, rs.lastCreateDebug.String()), "red")
		}
		return fail(err)
	}
	s.appendLog(fmt.Sprintf("[任务%d] 创建账号资料完成", index), "yellow")
	if rs, ok := provider.Session().(*registerSession); ok {
		s.appendLog(fmt.Sprintf("[任务%d] 创建账号资料完成调试[%s]", index, rs.lastCreateDebug.String()), "yellow")
	}
	oauthCode := parseContinueCode(continueURL)
	s.appendLog(fmt.Sprintf("[任务%d] 开始换 token", index), "yellow")
	tokens, err := provider.Session().exchangeTokens(ctx, codeVerifier, oauthCode)
	if err != nil {
		return fail(err)
	}
	s.appendLog(fmt.Sprintf("[任务%d] token 换取完成", index), "yellow")
	accessToken := strings.TrimSpace(strAny(tokens["access_token"], ""))
	if accessToken == "" {
		return fail(errors.New("token换取失败"))
	}
	refreshToken := strings.TrimSpace(strAny(tokens["refresh_token"], ""))
	idToken := strings.TrimSpace(strAny(tokens["id_token"], ""))
	now := nowISO()
	record := map[string]any{
		"access_token":  accessToken,
		"type":          "free",
		"status":        "异常",
		"mfa_status":    "pending",
		"quota":         0,
		"success":       0,
		"fail":          0,
		"email":         email,
		"password":      password,
		"refresh_token": refreshToken,
		"id_token":      idToken,
		"created_at":    now,
		"source_type":   "web",
	}
	if mailbox.ClientID != "" {
		record["client_id"] = mailbox.ClientID
	}
	if mailbox.AccountID != "" {
		record["account_id"] = mailbox.AccountID
	}
	if mailbox.Password != "" {
		record["email_password"] = mailbox.Password
	}
	if mailbox.Extra != nil {
		for k, v := range mailbox.Extra {
			if _, exists := record[k]; !exists {
				record[k] = v
			}
		}
	}
	if err := s.importRegisterAccount(record); err != nil {
		return false, fmt.Errorf("注册已完成但保存账号失败: %w", err)
	}
	rs, ok := provider.Session().(*registerSession)
	if !ok {
		_ = s.server.store.updateAccountMFA(accessToken, "failed", "")
		return false, fmt.Errorf("%s: 账号已保存但 2FA 未完成: 注册会话不可用", email)
	}
	s.appendLog(fmt.Sprintf("[任务%d] 开始设置验证器 2FA", index), "yellow")
	if err := s.enableRegisterMFA(ctx, rs, accessToken, deviceID); err != nil {
		return false, fmt.Errorf("%s: 账号已保存但 2FA 未完成: %w", email, err)
	}
	s.appendLog(fmt.Sprintf("[任务%d] %s 验证器 2FA 已启用并保存", index, email), "green")
	if s.server != nil {
		if _, errs := s.server.refreshAccountInfos(ctx, []string{accessToken}); len(errs) > 0 {
			s.appendLog(fmt.Sprintf("[任务%d] 账号已保存，刷新状态暂未成功: %s", index, strAny(errs[0]["error"], "未知错误")), "yellow")
		} else {
			for _, account := range s.server.store.LoadAccounts() {
				if account.AccessToken == accessToken {
					label := firstNonEmpty(account.TrialPromoLabel, "未判定")
					s.appendLog(fmt.Sprintf("[任务%d] %s 优惠检查: %s", index, email, label), "yellow")
					break
				}
			}
		}
	}
	s.appendLog(fmt.Sprintf("[任务%d] %s 注册成功", index, email), "green")
	return true, nil
}

func isRegisterContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func randomPassword() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%"
	b := make([]byte, 16)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func randomToken(n int) string {
	src := strings.ReplaceAll(uuid4()+uuid4(), "-", "")
	if n >= len(src) {
		return src
	}
	return src[:n]
}

type registerProviderSession interface {
	platformAuthorize(ctx context.Context, deviceID, email string) (string, error)
	platformAuthorizeForRegistration(ctx context.Context, deviceID, email string) (string, bool, error)
	platformAuthorizeEmailOTP(ctx context.Context, deviceID, email string) (string, error)
	authorizeContinue(ctx context.Context, deviceID, email string) error
	preparePasswordSignup(ctx context.Context, deviceID string) error
	registerUser(ctx context.Context, deviceID, email, password string) error
	sendOTP(ctx context.Context, deviceID string) error
	validateOTP(ctx context.Context, deviceID, code string) (string, error)
	visitAuthPage(ctx context.Context, rawURL, referer string) error
	createAccount(ctx context.Context, deviceID string, profile registerProfileConfig) (string, error)
	exchangeTokens(ctx context.Context, codeVerifier, code string) (map[string]any, error)
}

type registerCurlCookieSetter interface {
	SetCookies(rawURL string, cookies []*stdhttp.Cookie)
}

type registerProviderBase struct {
	sess *registerSession
}

func (b *registerProviderBase) Session() registerProviderSession { return b.sess }

func (b *registerProviderBase) MarkFailed(mailbox *registerMailbox, reason string) {}

func (s *registerService) createProvider(mail RegisterMailConfig) (registerProvider, error) {
	route, err := s.resolveRegisterProxy(s.snapshot().Proxy)
	if err != nil {
		return nil, err
	}
	return s.createProviderWithProxy(mail, route.URL)
}

func (s *registerService) resolveRegisterProxy(raw string) (proxyRoute, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "group:") {
		if s.server == nil {
			return proxyRoute{}, errors.New("注册代理组需要服务器连接")
		}
		return s.server.resolveProxyRoute(proxyReference{Mode: "group", GroupID: strings.TrimPrefix(raw, "group:")})
	}
	if raw == "" {
		return proxyRoute{}, nil
	}
	normalized, err := normalizeProxyURL(raw)
	if err != nil {
		return proxyRoute{}, err
	}
	return proxyRoute{URL: normalized}, nil
}

func (s *registerService) createProviderWithProxy(mail RegisterMailConfig, registerProxy string) (registerProvider, error) {
	entries := []map[string]any{}
	for _, raw := range mail.Providers {
		if raw == nil {
			continue
		}
		if enabled, ok := raw["enable"]; ok && !boolAny(enabled, true) {
			continue
		}
		item := map[string]any{}
		for k, v := range raw {
			item[k] = v
		}
		providerType := strings.ToLower(strings.TrimSpace(strAny(item["type"], "tempmail_lol")))
		if providerType == "" {
			providerType = "tempmail_lol"
		}
		if strings.TrimSpace(strAny(item["provider_ref"], "")) == "" {
			item["provider_ref"] = fmt.Sprintf("%s#%d", providerType, len(entries)+1)
		}
		if strings.TrimSpace(strAny(item["label"], "")) == "" {
			item["label"] = item["provider_ref"]
		}
		if item["wait_timeout"] == nil {
			item["wait_timeout"] = mail.WaitTimeout
		}
		if item["wait_interval"] == nil {
			item["wait_interval"] = mail.WaitInterval
		}
		entries = append(entries, item)
	}
	if len(entries) == 0 {
		return nil, errors.New("mail.providers 没有启用的 provider")
	}
	entry := s.nextProviderEntry(entries)
	if entry == nil {
		return nil, errors.New("mail.providers 没有可用 provider")
	}
	providerType := strings.ToLower(strings.TrimSpace(strAny(entry["type"], "")))
	if providerType == "" {
		providerType = "tempmail_lol"
	}
	proxy := ""
	if mail.APIUseRegisterProxy || providerType == "icloud_api" {
		proxy = registerProxy
	}
	var binGetter func() (string, error)
	if s.server != nil {
		binGetter = s.server.ensureCurlImpersonateBinary
	}
	sess, err := newRegisterSession(proxy, mail.RequestTimeout, binGetter)
	if err != nil {
		return nil, err
	}
	switch providerType {
	case "cloudflare_temp_email":
		return &cloudflareTempMailRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "ddg_mail":
		return &ddgMailRegisterProvider{
			registerProviderBase: registerProviderBase{sess: sess},
			entry:                entry,
			aliasesPath:          filepath.Join(filepath.Dir(s.path), "ddg_aliases.json"),
			store:                s.store,
		}, nil
	case "tempmail_lol":
		return &tempMailLolRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "duckmail":
		return &duckMailRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "gptmail":
		return &gptMailRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "moemail":
		return &moEmailRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "inbucket":
		return &inbucketRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "yyds_mail":
		return &yydsMailRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "cloudmail", "cloudmail_gen":
		return &cloudMailGenRegisterProvider{registerProviderBase{sess: sess}, entry}, nil
	case "icloud_api":
		provider := &iCloudAPIRegisterProvider{registerProviderBase: registerProviderBase{sess: sess}, entry: entry}
		if !mail.APIUseRegisterProxy {
			provider.mailSess, err = newRegisterSession("", mail.RequestTimeout, nil)
			if err != nil {
				sess.close()
				return nil, err
			}
		}
		return provider, nil
	case "outlook_token":
		return &outlookTokenRegisterProvider{
			registerProviderBase: registerProviderBase{sess: sess},
			entry:                entry,
			statePath:            filepath.Join(filepath.Dir(s.path), "outlook_token_used.json"),
			store:                s.store,
		}, nil
	default:
		return nil, fmt.Errorf("不支持的 mail.provider: %s", providerType)
	}
}

func waitForRegisterCode(ctx context.Context, mailbox *registerMailbox, timeoutSeconds, intervalSeconds int, fetch func() ([]map[string]any, error)) (string, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	if intervalSeconds <= 0 {
		intervalSeconds = 2
	}
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	interval := time.Duration(intervalSeconds) * time.Second
	lastCount := 0
	lastSkipped := 0
	lastContent := 0
	lastErr := ""
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		items, err := fetch()
		if err == nil {
			lastErr = ""
			lastCount = len(items)
			lastSkipped = 0
			lastContent = 0
			seenRefs := mailboxSeenCodeRefs(mailbox)
			for _, item := range items {
				if messageBeforeCodeBoundary(mailbox, item) {
					lastSkipped++
					continue
				}
				ref := messageTrackingRef(item)
				if ref != "" && seenRefs[ref] {
					continue
				}
				if registerMessageHasContent(item) {
					lastContent++
				}
				if code := extractRegisterCode(item); code != "" {
					mailboxRememberCodeRef(mailbox, ref)
					return code, nil
				}
				if ref != "" {
					seenRefs[ref] = true
				}
			}
		} else {
			lastErr = err.Error()
		}
		if !sleepContext(ctx, interval) {
			return "", ctx.Err()
		}
	}
	parts := []string{fmt.Sprintf("最近轮询邮件%d封", lastCount)}
	if lastSkipped > 0 {
		parts = append(parts, fmt.Sprintf("跳过旧邮件%d封", lastSkipped))
	}
	parts = append(parts, fmt.Sprintf("有内容邮件%d封", lastContent))
	if lastErr != "" {
		parts = append(parts, "最近错误: "+lastErr)
	}
	return "", fmt.Errorf("等待注册验证码超时（%s）", strings.Join(parts, "，"))
}

type cloudflareTempMailRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *cloudflareTempMailRegisterProvider) Name() string { return "cloudflare_temp_email" }

func (p *cloudflareTempMailRegisterProvider) Close() { p.sess.close() }

func (p *cloudflareTempMailRegisterProvider) request(ctx context.Context, method, path string, headers map[string]string, params url.Values, payload any, out any, expected ...int) error {
	apiBase := strings.TrimRight(strings.TrimSpace(strAny(p.entry["api_base"], "")), "/")
	if apiBase == "" {
		return errors.New("CloudflareTempMail 缺少 api_base")
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		merged[k] = v
	}
	rawURL := apiBase + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	return p.sess.requestJSON(ctx, method, rawURL, merged, payload, out, expected...)
}

func (p *cloudflareTempMailRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	adminPassword := strings.TrimSpace(strAny(p.entry["admin_password"], ""))
	if adminPassword == "" {
		return nil, errors.New("CloudflareTempMail 缺少 admin_password")
	}
	payload := map[string]any{"enablePrefix": true, "name": firstNonEmpty(username, randomMailboxName())}
	if domains := normalizeStringList(p.entry["domain"]); len(domains) > 0 {
		payload["domain"] = domains[rand.Intn(len(domains))]
	}
	var data map[string]any
	if err := p.request(context.Background(), stdhttp.MethodPost, "/admin/new_address", map[string]string{"x-admin-auth": adminPassword}, nil, payload, &data, 200, 201); err != nil {
		return nil, err
	}
	address := strings.TrimSpace(strAny(data["address"], ""))
	token := strings.TrimSpace(strAny(data["jwt"], ""))
	if address == "" || token == "" {
		return nil, errors.New("CloudflareTempMail 缺少 address 或 jwt")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address, Token: token}, nil
}

func (p *cloudflareTempMailRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		var data any
		if err := p.request(ctx, stdhttp.MethodGet, "/api/mails", map[string]string{"Authorization": "Bearer " + mailbox.Token}, url.Values{"limit": []string{"10"}, "offset": []string{"0"}}, nil, &data, 200); err != nil {
			return nil, err
		}
		items := genericMailItems(data)
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if messageMatchesEmail(item, mailbox.Address) {
				out = append(out, normalizeGenericMailMessage(item))
			}
		}
		return out, nil
	})
}

type ddgMailRegisterProvider struct {
	registerProviderBase
	entry       map[string]any
	aliasesPath string
	store       *Store
	aliasMu     sync.Mutex
}

func (p *ddgMailRegisterProvider) Name() string { return "ddg_mail" }

func (p *ddgMailRegisterProvider) Close() { p.sess.close() }

func (p *ddgMailRegisterProvider) ddgRequest(ctx context.Context, method, path string, payload any, out any) error {
	token := strings.TrimSpace(strAny(p.entry["ddg_token"], ""))
	if token == "" {
		return errors.New("DDGMail 缺少 ddg_token")
	}
	return p.sess.requestJSON(ctx, method, "https://quack.duckduckgo.com"+path, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}, payload, out, 200, 201)
}

func (p *ddgMailRegisterProvider) cfHeaders(contentType bool) map[string]string {
	headers := map[string]string{}
	if contentType {
		headers["Content-Type"] = "application/json"
	}
	apiKey := strings.TrimSpace(strAny(p.entry["cf_api_key"], ""))
	authMode := strings.ToLower(strings.TrimSpace(strAny(p.entry["cf_auth_mode"], "none")))
	if apiKey != "" {
		if authMode == "x-api-key" {
			headers["X-API-Key"] = apiKey
		} else if authMode != "none" && authMode != "query-key" {
			headers["Authorization"] = "Bearer " + apiKey
		}
	}
	return headers
}

func (p *ddgMailRegisterProvider) cfRequest(ctx context.Context, method, path string, headers map[string]string, params url.Values, payload any, out any, expected ...int) error {
	apiBase := strings.TrimRight(firstNonEmpty(strAny(p.entry["api_base"], ""), strAny(p.entry["cf_api_base"], "")), "/")
	if apiBase == "" {
		return errors.New("DDGMail 缺少 api_base/cf_api_base")
	}
	merged := p.cfHeaders(true)
	for k, v := range headers {
		merged[k] = v
	}
	if adminPassword := strings.TrimSpace(strAny(p.entry["admin_password"], "")); adminPassword != "" && strings.EqualFold(method, stdhttp.MethodPost) {
		merged["x-admin-auth"] = adminPassword
	}
	if apiKey := strings.TrimSpace(strAny(p.entry["cf_api_key"], "")); apiKey != "" && strings.EqualFold(strAny(p.entry["cf_auth_mode"], ""), "query-key") {
		if params == nil {
			params = url.Values{}
		}
		params.Set("key", apiKey)
	}
	rawURL := apiBase + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	return p.sess.requestJSON(ctx, method, rawURL, merged, payload, out, expected...)
}

func (p *ddgMailRegisterProvider) loadAliases() map[string]bool {
	out := map[string]bool{}
	if p.store != nil {
		var arr []string
		if err := p.store.LoadDocument("ddg_aliases.json", &arr); err != nil {
			return out
		}
		for _, item := range arr {
			if key := strings.ToLower(strings.TrimSpace(item)); key != "" {
				out[key] = true
			}
		}
		return out
	}
	b, err := os.ReadFile(p.aliasesPath)
	if err != nil || len(b) == 0 {
		return out
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		for _, item := range arr {
			if key := strings.ToLower(strings.TrimSpace(item)); key != "" {
				out[key] = true
			}
		}
	}
	return out
}

func (p *ddgMailRegisterProvider) recordAlias(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return false
	}
	p.aliasMu.Lock()
	defer p.aliasMu.Unlock()
	aliases := p.loadAliases()
	if aliases[address] {
		return false
	}
	aliases[address] = true
	items := make([]string, 0, len(aliases))
	for item := range aliases {
		items = append(items, item)
	}
	sort.Strings(items)
	if p.store != nil {
		if err := p.store.SaveDocument("ddg_aliases.json", items); err != nil {
			return false
		}
	} else {
		if err := writeJSONFile(p.aliasesPath, items); err != nil {
			return false
		}
	}
	return true
}

func (p *ddgMailRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	var data map[string]any
	if err := p.ddgRequest(context.Background(), stdhttp.MethodPost, "/api/email/addresses", map[string]any{}, &data); err != nil {
		return nil, err
	}
	part := strings.TrimSpace(strAny(data["address"], ""))
	if part == "" {
		return nil, errors.New("DDG API 返回无 address 字段")
	}
	address := part + "@duck.com"
	if !p.recordAlias(address) {
		return nil, fmt.Errorf("[%s] DDG日上限已达，别名 %s 已存在，自动切换邮箱提供商", firstNonEmpty(strAny(p.entry["label"], ""), strAny(p.entry["provider_ref"], "")), address)
	}
	token := strings.TrimSpace(strAny(p.entry["cf_inbox_jwt"], ""))
	if token == "" {
		return nil, errors.New("DDGMail 需要 cf_inbox_jwt")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Label: strAny(p.entry["label"], ""), Address: address, Token: token}, nil
}

func (p *ddgMailRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		path := strings.TrimSpace(strAny(p.entry["cf_messages_path"], "/api/mails"))
		if path == "" {
			path = "/api/mails"
		}
		var data any
		if err := p.cfRequest(ctx, stdhttp.MethodGet, path, map[string]string{"Authorization": "Bearer " + mailbox.Token}, url.Values{"limit": []string{"30"}, "offset": []string{"0"}}, nil, &data, 200); err != nil {
			return nil, err
		}
		items := genericMailItems(data)
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if rawRecipient := parseRawRecipient(strAny(item["raw"], "")); rawRecipient != "" && !strings.Contains(rawRecipient, strings.ToLower(mailbox.Address)) {
				continue
			}
			out = append(out, normalizeGenericMailMessage(item))
		}
		return out, nil
	})
}

type tempMailLolRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *tempMailLolRegisterProvider) Name() string { return "tempmail_lol" }

func (p *tempMailLolRegisterProvider) Close() { p.sess.close() }

func (p *tempMailLolRegisterProvider) headers() map[string]string {
	headers := map[string]string{}
	if apiKey := strings.TrimSpace(strAny(p.entry["api_key"], "")); apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	return headers
}

func (p *tempMailLolRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	domains := normalizeStringList(p.entry["domain"])
	payload := map[string]any{}
	if len(domains) > 0 {
		domain := domains[rand.Intn(len(domains))]
		if strings.HasPrefix(strings.ToLower(domain), "*.") && len(domain) > 2 {
			payload["domain"] = randomSubdomainLabel() + "." + strings.TrimPrefix(strings.ToLower(domain), "*.")
			payload["prefix"] = randomMailboxName()
		} else {
			payload["domain"] = domain
		}
	}
	if username != "" && payload["prefix"] == nil {
		payload["prefix"] = username
	}
	var data map[string]any
	if err := p.sess.requestJSON(context.Background(), stdhttp.MethodPost, "https://api.tempmail.lol/v2/inbox/create", p.headers(), payload, &data, 200, 201); err != nil {
		return nil, err
	}
	address := strings.TrimSpace(strAny(data["address"], ""))
	token := strings.TrimSpace(strAny(data["token"], ""))
	if address == "" || token == "" {
		return nil, errors.New("TempMail.lol 缺少 address 或 token")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address, Token: token}, nil
}

func (p *tempMailLolRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		var data map[string]any
		if err := p.sess.requestJSON(ctx, stdhttp.MethodGet, "https://api.tempmail.lol/v2/inbox?token="+url.QueryEscape(mailbox.Token), p.headers(), nil, &data, 200); err != nil {
			return nil, err
		}
		out := []map[string]any{}
		items := extractMessageItems(data)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				out = append(out, normalizeGenericMailMessage(m))
			}
		}
		return out, nil
	})
}

type duckMailRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *duckMailRegisterProvider) Name() string { return "duckmail" }

func (p *duckMailRegisterProvider) Close() { p.sess.close() }

func (p *duckMailRegisterProvider) request(ctx context.Context, method, path, token string, useAPIKey bool, params url.Values, payload any, out any, expected ...int) error {
	headers := map[string]string{"Content-Type": "application/json"}
	if useAPIKey {
		token = strings.TrimSpace(strAny(p.entry["api_key"], ""))
	}
	if strings.TrimSpace(token) != "" {
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
	}
	rawURL := "https://api.duckmail.sbs" + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	return p.sess.requestJSON(ctx, method, rawURL, headers, payload, out, expected...)
}

func (p *duckMailRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	apiKey := strings.TrimSpace(strAny(p.entry["api_key"], ""))
	if apiKey == "" {
		return nil, errors.New("DuckMail 缺少 api_key")
	}
	domain := strings.TrimSpace(strAny(p.entry["default_domain"], "duckmail.sbs"))
	if domain == "" {
		domain = "duckmail.sbs"
	}
	password := randomToken(12)
	address := fmt.Sprintf("%s@%s", firstNonEmpty(username, randomMailboxName()), domain)
	payload := map[string]any{"address": address, "password": password}
	var account map[string]any
	if err := p.request(context.Background(), stdhttp.MethodPost, "/accounts", "", true, nil, payload, &account, 200, 201); err != nil {
		return nil, err
	}
	var tokenData map[string]any
	if err := p.request(context.Background(), stdhttp.MethodPost, "/token", "", true, nil, payload, &tokenData, 200, 201); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(strAny(tokenData["token"], ""))
	if token == "" {
		return nil, errors.New("DuckMail 返回缺少 token")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address, Token: token, Password: password, AccountID: strAny(account["id"], "")}, nil
}

func (p *duckMailRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		var data any
		if err := p.request(ctx, stdhttp.MethodGet, "/messages", mailbox.Token, false, url.Values{"page": []string{"1"}}, nil, &data, 200, 201, 204); err != nil {
			return nil, err
		}
		items := duckMailItems(data)
		if len(items) == 0 {
			return nil, nil
		}
		item := items[0]
		messageID := strings.TrimPrefix(strAny(item["id"], strAny(item["@id"], "")), "/messages/")
		if messageID != "" {
			var detail map[string]any
			if err := p.request(ctx, stdhttp.MethodGet, "/messages/"+url.PathEscape(messageID), mailbox.Token, false, nil, nil, &detail, 200); err == nil && detail != nil {
				item = detail
			}
		}
		return []map[string]any{normalizeGenericMailMessage(item)}, nil
	})
}

type gptMailRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *gptMailRegisterProvider) Name() string { return "gptmail" }

func (p *gptMailRegisterProvider) Close() { p.sess.close() }

func (p *gptMailRegisterProvider) authHeaders() map[string]string {
	headers := map[string]string{"Content-Type": "application/json"}
	if apiKey := strings.TrimSpace(strAny(p.entry["api_key"], "")); apiKey != "" {
		headers["X-API-Key"] = apiKey
		authMode := strings.ToLower(strings.TrimSpace(strAny(p.entry["auth_mode"], "both")))
		if authMode == "" || authMode == "both" || authMode == "bearer" || authMode == "authorization" {
			headers["Authorization"] = "Bearer " + apiKey
		}
	}
	return headers
}

func (p *gptMailRegisterProvider) request(ctx context.Context, method, path string, params url.Values, payload any, out any) error {
	rawURL := "https://mail.chatgpt.org.uk" + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	headers := p.authHeaders()
	var data any
	if out == nil {
		out = &data
	}
	if err := p.sess.requestJSON(ctx, method, rawURL, headers, payload, out, 200); err != nil {
		return err
	}
	if ptr, ok := out.(*any); ok {
		*ptr = unwrapDataEnvelope(*ptr)
	}
	return nil
}

func (p *gptMailRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	payload := map[string]any{}
	if strings.TrimSpace(username) != "" {
		payload["prefix"] = strings.TrimSpace(username)
	}
	if domain := strings.TrimSpace(strAny(p.entry["default_domain"], "")); domain != "" {
		payload["domain"] = domain
	}
	method := stdhttp.MethodGet
	var body any
	if len(payload) > 0 {
		method = stdhttp.MethodPost
		body = payload
	}
	var data any
	if err := p.request(context.Background(), method, "/api/generate-email", nil, body, &data); err != nil {
		return nil, err
	}
	m, _ := unwrapDataEnvelope(data).(map[string]any)
	address := strings.TrimSpace(strAny(m["email"], ""))
	if address == "" {
		return nil, errors.New("GPTMail 返回缺少 email")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address}, nil
}

func (p *gptMailRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		var data any
		if err := p.request(ctx, stdhttp.MethodGet, "/api/emails", url.Values{"email": []string{mailbox.Address}}, nil, &data); err != nil {
			return nil, err
		}
		items := genericMailItems(unwrapDataEnvelope(data))
		if len(items) == 0 {
			return nil, nil
		}
		item := latestMailItem(items)
		if id := strings.TrimSpace(strAny(item["id"], "")); id != "" {
			var detail any
			if err := p.request(ctx, stdhttp.MethodGet, "/api/email/"+url.PathEscape(id), nil, nil, &detail); err == nil {
				if m, ok := unwrapDataEnvelope(detail).(map[string]any); ok {
					item = m
				}
			}
		}
		return []map[string]any{normalizeGenericMailMessage(item)}, nil
	})
}

type moEmailRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *moEmailRegisterProvider) Name() string { return "moemail" }

func (p *moEmailRegisterProvider) Close() { p.sess.close() }

func (p *moEmailRegisterProvider) request(ctx context.Context, method, path string, params url.Values, payload any, out any, expected ...int) error {
	apiBase := strings.TrimRight(strings.TrimSpace(strAny(p.entry["api_base"], "")), "/")
	if apiBase == "" {
		return errors.New("MoEmail 缺少 api_base")
	}
	rawURL := apiBase + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if apiKey := strings.TrimSpace(strAny(p.entry["api_key"], "")); apiKey != "" {
		headers["X-API-Key"] = apiKey
	}
	return p.sess.requestJSON(ctx, method, rawURL, headers, payload, out, expected...)
}

func (p *moEmailRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	payload := map[string]any{"name": firstNonEmpty(username, randomMailboxName()), "expiryTime": intAny(p.entry["expiry_time"], 0)}
	if domains := normalizeStringList(p.entry["domain"]); len(domains) > 0 {
		payload["domain"] = domains[rand.Intn(len(domains))]
	}
	var data map[string]any
	if err := p.request(context.Background(), stdhttp.MethodPost, "/api/emails/generate", nil, payload, &data, 200, 201); err != nil {
		return nil, err
	}
	address := strings.TrimSpace(strAny(data["email"], ""))
	emailID := strings.TrimSpace(strAny(data["id"], strAny(data["email_id"], "")))
	if address == "" || emailID == "" {
		return nil, errors.New("MoEmail 缺少 email 或 id")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address, AccountID: emailID, Extra: map[string]any{"email_id": emailID}}, nil
}

func (p *moEmailRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		extraEmailID := ""
		if mailbox.Extra != nil {
			extraEmailID = strAny(mailbox.Extra["email_id"], "")
		}
		emailID := firstNonEmpty(mailbox.AccountID, extraEmailID)
		var data map[string]any
		if err := p.request(ctx, stdhttp.MethodGet, "/api/emails/"+url.PathEscape(emailID), nil, nil, &data, 200); err != nil {
			return nil, err
		}
		items := genericMailItems(data["messages"])
		if len(items) == 0 {
			return nil, nil
		}
		item := latestMailItem(items)
		messageID := strings.TrimSpace(strAny(item["id"], strAny(item["message_id"], strAny(item["_id"], ""))))
		if messageID != "" {
			var detail map[string]any
			if err := p.request(ctx, stdhttp.MethodGet, "/api/emails/"+url.PathEscape(emailID)+"/"+url.PathEscape(messageID), nil, nil, &detail, 200); err == nil && detail != nil {
				if msg, ok := detail["message"].(map[string]any); ok {
					item = msg
				} else {
					item = detail
				}
			}
		}
		return []map[string]any{normalizeGenericMailMessage(item)}, nil
	})
}

type inbucketRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *inbucketRegisterProvider) Name() string { return "inbucket" }

func (p *inbucketRegisterProvider) Close() { p.sess.close() }

func (p *inbucketRegisterProvider) request(ctx context.Context, method, path string, out any, expected ...int) error {
	apiBase := strings.TrimRight(strings.TrimSpace(strAny(p.entry["api_base"], "")), "/")
	if apiBase == "" {
		return errors.New("Inbucket 缺少 api_base")
	}
	return p.sess.requestJSON(ctx, method, apiBase+path, map[string]string{}, nil, out, expected...)
}

func (p *inbucketRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	domains := normalizeStringList(p.entry["domain"])
	if len(domains) == 0 {
		return nil, errors.New("Inbucket 需要至少配置一个 domain")
	}
	domain := domains[rand.Intn(len(domains))]
	if boolAny(p.entry["random_subdomain"], true) {
		domain = randomSubdomainLabel() + "." + domain
	}
	localPart := firstNonEmpty(username, randomMailboxName())
	address := localPart + "@" + domain
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address, MailboxName: localPart, Extra: map[string]any{"base_domain": domain}}, nil
}

func (p *inbucketRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		mailboxName := firstNonEmpty(mailbox.MailboxName, strings.Split(mailbox.Address, "@")[0])
		var data any
		if err := p.request(ctx, stdhttp.MethodGet, "/api/v1/mailbox/"+url.PathEscape(mailboxName), &data, 200); err != nil {
			return nil, err
		}
		items := genericMailItems(data)
		out := make([]map[string]any, 0, len(items))
		for _, summary := range items {
			messageID := strings.TrimSpace(strAny(summary["id"], ""))
			if messageID == "" {
				continue
			}
			var detail map[string]any
			if err := p.request(ctx, stdhttp.MethodGet, "/api/v1/mailbox/"+url.PathEscape(mailboxName)+"/"+url.PathEscape(messageID), &detail, 200); err != nil {
				continue
			}
			msg := normalizeInbucketMessage(summary, detail)
			if messageMatchesEmail(msg, mailbox.Address) {
				out = append(out, msg)
			}
		}
		return out, nil
	})
}

type yydsMailRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *yydsMailRegisterProvider) Name() string { return "yyds_mail" }

func (p *yydsMailRegisterProvider) Close() { p.sess.close() }

func (p *yydsMailRegisterProvider) request(ctx context.Context, method, path, token string, params url.Values, payload any, out any, expected ...int) error {
	apiBase := strings.TrimRight(strings.TrimSpace(strAny(p.entry["api_base"], "https://maliapi.215.im/v1")), "/")
	headers := map[string]string{"Content-Type": "application/json"}
	if strings.TrimSpace(token) != "" {
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
	} else if apiKey := strings.TrimSpace(strAny(p.entry["api_key"], "")); apiKey != "" {
		headers["X-API-Key"] = apiKey
	}
	rawURL := apiBase + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	var data any
	target := out
	if target == nil {
		target = &data
	}
	if err := p.sess.requestJSON(ctx, method, rawURL, headers, payload, target, expected...); err != nil {
		return err
	}
	return nil
}

func (p *yydsMailRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	payload := map[string]any{"localPart": firstNonEmpty(username, randomMailboxName())}
	if domains := normalizeStringList(p.entry["domain"]); len(domains) > 0 {
		payload["domain"] = domains[rand.Intn(len(domains))]
	}
	if subdomain := strings.TrimSpace(strAny(p.entry["subdomain"], "")); subdomain != "" {
		payload["subdomain"] = subdomain
	}
	path := "/accounts"
	if boolAny(p.entry["wildcard"], false) {
		path = "/accounts/wildcard"
	}
	var data any
	if err := p.request(context.Background(), stdhttp.MethodPost, path, "", nil, payload, &data, 200, 201, 204); err != nil {
		return nil, err
	}
	m, _ := unwrapDataEnvelope(data).(map[string]any)
	address := strings.TrimSpace(firstNonEmpty(strAny(m["address"], ""), strAny(m["email"], "")))
	token := strings.TrimSpace(firstNonEmpty(strAny(m["token"], ""), strAny(m["temp_token"], ""), strAny(m["tempToken"], ""), strAny(m["access_token"], "")))
	if address == "" || token == "" {
		return nil, errors.New("YYDSMail 缺少 address 或 token")
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address, Token: token, AccountID: strAny(m["id"], "")}, nil
}

func (p *yydsMailRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		var data any
		if err := p.request(ctx, stdhttp.MethodGet, "/messages", mailbox.Token, url.Values{"address": []string{mailbox.Address}}, nil, &data, 200, 201, 204); err != nil {
			return nil, err
		}
		items := genericMailItems(unwrapDataEnvelope(data))
		if len(items) == 0 {
			return nil, nil
		}
		item := latestMailItem(items)
		messageID := strings.TrimSpace(firstNonEmpty(strAny(item["id"], ""), strAny(item["message_id"], "")))
		if messageID != "" {
			var detail any
			if err := p.request(ctx, stdhttp.MethodGet, "/messages/"+url.PathEscape(messageID), mailbox.Token, url.Values{"address": []string{mailbox.Address}}, nil, &detail, 200, 201, 204); err == nil {
				if m, ok := unwrapDataEnvelope(detail).(map[string]any); ok {
					item = m
				}
			}
		}
		return []map[string]any{normalizeGenericMailMessage(item)}, nil
	})
}

type cloudMailGenRegisterProvider struct {
	registerProviderBase
	entry map[string]any
}

func (p *cloudMailGenRegisterProvider) Name() string { return "cloudmail_gen" }

func (p *cloudMailGenRegisterProvider) Close() { p.sess.close() }

func (p *cloudMailGenRegisterProvider) request(ctx context.Context, method, path string, headers map[string]string, payload any, out any, expected ...int) error {
	apiBase := strings.TrimRight(strings.TrimSpace(strAny(p.entry["api_base"], "")), "/")
	if apiBase == "" {
		return errors.New("CloudMailGen 缺少 api_base")
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		merged[k] = v
	}
	return p.sess.requestJSON(ctx, method, apiBase+path, merged, payload, out, expected...)
}

func (p *cloudMailGenRegisterProvider) tokenCacheKey() string {
	return strings.TrimRight(strings.TrimSpace(strAny(p.entry["api_base"], "")), "/") + "|" + strings.TrimSpace(strAny(p.entry["admin_email"], ""))
}

func (p *cloudMailGenRegisterProvider) clearTokenCache() {
	cloudMailTokenMu.Lock()
	defer cloudMailTokenMu.Unlock()
	delete(cloudMailTokenCache, p.tokenCacheKey())
}

func (p *cloudMailGenRegisterProvider) getToken(ctx context.Context) (string, error) {
	adminEmail := strings.TrimSpace(strAny(p.entry["admin_email"], ""))
	adminPassword := strings.TrimSpace(strAny(p.entry["admin_password"], ""))
	if adminEmail == "" || adminPassword == "" {
		return "", errors.New("CloudMailGen 缺少 admin_email 或 admin_password")
	}
	key := p.tokenCacheKey()
	cloudMailTokenMu.Lock()
	cached := cloudMailTokenCache[key]
	if cached.token != "" && time.Now().Before(cached.expiresAt.Add(-5*time.Minute)) {
		cloudMailTokenMu.Unlock()
		return cached.token, nil
	}
	cloudMailTokenMu.Unlock()
	var data map[string]any
	if err := p.request(ctx, stdhttp.MethodPost, "/api/public/genToken", nil, map[string]any{"email": adminEmail, "password": adminPassword}, &data, 200); err != nil {
		return "", err
	}
	token := ""
	if intAny(data["code"], 0) == 200 {
		if m, ok := data["data"].(map[string]any); ok {
			token = strings.TrimSpace(strAny(m["token"], ""))
		}
	}
	if token == "" {
		return "", fmt.Errorf("CloudMailGen genToken 返回异常: %v", data)
	}
	cloudMailTokenMu.Lock()
	cloudMailTokenCache[key] = cloudMailTokenCacheItem{token: token, expiresAt: time.Now().Add(24 * time.Hour)}
	cloudMailTokenMu.Unlock()
	return token, nil
}

func (p *cloudMailGenRegisterProvider) resolveAddress(username string) (string, error) {
	domains := normalizeStringList(p.entry["domain"])
	if len(domains) == 0 {
		return "", errors.New("CloudMailGen 需要至少配置一个 domain")
	}
	domain := domains[rand.Intn(len(domains))]
	if subdomains := normalizeStringList(p.entry["subdomain"]); len(subdomains) > 0 {
		domain = subdomains[rand.Intn(len(subdomains))] + "." + domain
	}
	localPart := strings.TrimSpace(username)
	if localPart == "" {
		if prefix := strings.TrimSpace(strAny(p.entry["email_prefix"], "")); prefix != "" {
			localPart = prefix + "_" + randomMailboxName()
		} else {
			localPart = randomMailboxName()
		}
	}
	return localPart + "@" + domain, nil
}

func (p *cloudMailGenRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	address, err := p.resolveAddress(username)
	if err != nil {
		return nil, err
	}
	token, err := p.getToken(context.Background())
	if err != nil {
		return nil, err
	}
	if err := p.request(context.Background(), stdhttp.MethodPost, "/api/public/addUser", map[string]string{"Authorization": token}, map[string]any{"list": []map[string]any{{"email": address}}}, nil, 200); err != nil {
		return nil, err
	}
	return &registerMailbox{Provider: p.Name(), ProviderRef: strAny(p.entry["provider_ref"], ""), Address: address}, nil
}

func (p *cloudMailGenRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	return waitForRegisterCode(ctx, mailbox, intAny(p.entry["wait_timeout"], 30), intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		token, err := p.getToken(ctx)
		if err != nil {
			return nil, err
		}
		var data map[string]any
		err = p.request(ctx, stdhttp.MethodPost, "/api/public/emailList", map[string]string{"Authorization": token}, map[string]any{"toEmail": mailbox.Address, "size": 20, "timeSort": "desc"}, &data, 200)
		if err != nil || intAny(data["code"], 0) != 200 {
			p.clearTokenCache()
			token, tokenErr := p.getToken(ctx)
			if tokenErr != nil {
				return nil, firstErr(err, tokenErr)
			}
			data = map[string]any{}
			if err = p.request(ctx, stdhttp.MethodPost, "/api/public/emailList", map[string]string{"Authorization": token}, map[string]any{"toEmail": mailbox.Address, "size": 20, "timeSort": "desc"}, &data, 200); err != nil {
				return nil, err
			}
		}
		if intAny(data["code"], 0) != 200 {
			return nil, fmt.Errorf("CloudMailGen emailList 返回异常: %v", data)
		}
		items := genericMailItems(data["data"])
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if messageMatchesEmail(item, mailbox.Address) {
				out = append(out, normalizeGenericMailMessage(item))
			}
		}
		return out, nil
	})
}

type outlookTokenRegisterProvider struct {
	registerProviderBase
	entry       map[string]any
	statePath   string
	store       *Store
	stateMu     sync.Mutex
	credentials []map[string]string
}

func (p *outlookTokenRegisterProvider) Name() string { return "outlook_token" }

func (p *outlookTokenRegisterProvider) Close() { p.sess.close() }

func (p *outlookTokenRegisterProvider) MarkFailed(mailbox *registerMailbox, reason string) {
	if mailbox == nil || strings.TrimSpace(mailbox.Address) == "" {
		return
	}
	p.markOutlookState(mailbox.Address, "failed", reason)
}

func (p *outlookTokenRegisterProvider) ensurePool() {
	if p.credentials != nil {
		return
	}
	p.credentials = parseOutlookCredentials(strAny(p.entry["mailboxes"], ""))
}

func (p *outlookTokenRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	p.ensurePool()
	if len(p.credentials) == 0 {
		return nil, errors.New("OutlookToken 邮箱池为空")
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	state := p.loadState()
	var cred map[string]string
	for _, item := range p.credentials {
		if outlookCredentialAvailable(state[strings.ToLower(strings.TrimSpace(item["email"]))]) {
			cred = item
			break
		}
	}
	if cred == nil {
		return nil, fmt.Errorf("OutlookToken 邮箱池暂无可用邮箱（共 %d 个）", len(p.credentials))
	}
	state[strings.ToLower(strings.TrimSpace(cred["email"]))] = map[string]any{"state": "in_use", "reason": "", "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	p.saveState(state)
	return &registerMailbox{
		Provider:    p.Name(),
		ProviderRef: strAny(p.entry["provider_ref"], ""),
		Address:     cred["email"],
		Token:       cred["refresh_token"],
		Password:    cred["password"],
		ClientID:    cred["client_id"],
		Label:       strAny(p.entry["label"], ""),
	}, nil
}

func (p *outlookTokenRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	clientID := strings.TrimSpace(mailbox.ClientID)
	refreshToken := strings.TrimSpace(mailbox.Token)
	if clientID == "" || refreshToken == "" {
		return "", errors.New("OutlookToken mailbox 缺少 client_id 或 refresh_token")
	}
	mode := strings.ToLower(strings.TrimSpace(strAny(p.entry["mode"], "graph")))
	if mode != "graph" {
		mode = "graph"
	}
	timeout := 30 * time.Second
	if v := intAny(p.entry["wait_timeout"], 0); v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	interval := 2 * time.Second
	if v := intAny(p.entry["wait_interval"], 0); v > 0 {
		interval = time.Duration(v) * time.Second
	}
	accessToken, err := p.exchangeRefreshToken(clientID, refreshToken, "offline_access https://graph.microsoft.com/Mail.Read")
	if err != nil {
		p.markOutlookState(mailbox.Address, "token_invalid", err.Error())
		return "", err
	}
	deadline := time.Now().Add(timeout)
	lastCount := 0
	lastSkipped := 0
	lastContent := 0
	lastErr := ""
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		items, err := p.readGraphMessages(accessToken)
		if err != nil {
			lastErr = err.Error()
			if !sleepContext(ctx, interval) {
				return "", ctx.Err()
			}
			continue
		}
		lastErr = ""
		lastCount = len(items)
		lastSkipped = 0
		lastContent = 0
		for _, item := range items {
			if messageBeforeCodeBoundary(mailbox, item) {
				lastSkipped++
				continue
			}
			if registerMessageHasContent(item) {
				lastContent++
			}
			code := extractRegisterCode(item)
			if code != "" {
				p.markOutlookState(mailbox.Address, "used", "")
				return code, nil
			}
		}
		if !sleepContext(ctx, interval) {
			return "", ctx.Err()
		}
	}
	parts := []string{fmt.Sprintf("最近轮询邮件%d封", lastCount)}
	if lastSkipped > 0 {
		parts = append(parts, fmt.Sprintf("跳过旧邮件%d封", lastSkipped))
	}
	parts = append(parts, fmt.Sprintf("有内容邮件%d封", lastContent))
	if lastErr != "" {
		parts = append(parts, "最近错误: "+lastErr)
	}
	reason := fmt.Sprintf("等待注册验证码超时（%s）", strings.Join(parts, "，"))
	p.markOutlookState(mailbox.Address, "failed", reason)
	return "", errors.New(reason)
}

func (p *outlookTokenRegisterProvider) exchangeRefreshToken(clientID, refreshToken, scope string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", scope)
	req, err := stdhttp.NewRequest(stdhttp.MethodPost, outlookTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", p.sess.userAgent)
	resp, err := p.sess.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OutlookToken 刷新失败: HTTP %d", resp.StatusCode)
	}
	token := strings.TrimSpace(strAny(out["access_token"], ""))
	if token == "" {
		return "", errors.New("OutlookToken 刷新响应缺少 access_token")
	}
	return token, nil
}

func (p *outlookTokenRegisterProvider) readGraphMessages(accessToken string) ([]map[string]any, error) {
	rawURL := "https://graph.microsoft.com/v1.0/me/messages?$top=10&$orderby=receivedDateTime desc&$select=subject,receivedDateTime,from,body,bodyPreview"
	resp, err := p.sess.do(context.Background(), stdhttp.MethodGet, rawURL, map[string]string{
		"Authorization": "Bearer " + accessToken,
		"Accept":        "application/json",
	}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("OutlookToken Graph 失败: HTTP %d, %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	items := extractMessageItems(data)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, normalizeGraphItem(m))
		}
	}
	return out, nil
}

func (p *outlookTokenRegisterProvider) markOutlookState(address, state, reason string) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	store := p.loadState()
	key := strings.ToLower(strings.TrimSpace(address))
	store[key] = map[string]any{"state": state, "reason": reason, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	p.saveState(store)
}

func (p *outlookTokenRegisterProvider) loadState() map[string]map[string]any {
	if p.store == nil {
		return loadOutlookState(p.statePath)
	}
	state := map[string]map[string]any{}
	_ = p.store.LoadDocument("outlook_token_used.json", &state)
	return state
}

func (p *outlookTokenRegisterProvider) saveState(state map[string]map[string]any) {
	if p.store == nil {
		saveOutlookState(p.statePath, state)
		return
	}
	_ = p.store.SaveDocument("outlook_token_used.json", state)
}

func loadOutlookState(path string) map[string]map[string]any {
	out := map[string]map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

func saveOutlookState(path string, state map[string]map[string]any) {
	_ = writeJSONFile(path, state)
}

func (s *registerService) resetOutlookPoolState(scope string) int {
	path := filepath.Join(filepath.Dir(s.path), "outlook_token_used.json")
	state := map[string]map[string]any{}
	if s.store != nil {
		_ = s.store.LoadDocument("outlook_token_used.json", &state)
	} else {
		state = loadOutlookState(path)
	}
	if len(state) == 0 {
		return 0
	}
	if strings.TrimSpace(scope) == "failed" {
		removed := 0
		for key, value := range state {
			switch strings.ToLower(strings.TrimSpace(strAny(value["state"], ""))) {
			case "failed", "token_invalid", "in_use":
				delete(state, key)
				removed++
			}
		}
		if s.store != nil {
			_ = s.store.SaveDocument("outlook_token_used.json", state)
		} else {
			saveOutlookState(path, state)
		}
		return removed
	}
	removed := len(state)
	if s.store != nil {
		_ = s.store.SaveDocument("outlook_token_used.json", map[string]map[string]any{})
	} else {
		saveOutlookState(path, map[string]map[string]any{})
	}
	return removed
}

func parseOutlookCredentials(raw string) []map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if json.Valid([]byte(raw)) {
		var arr []map[string]any
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := make([]map[string]string, 0, len(arr))
			for _, item := range arr {
				out = append(out, outlookCredentialFromAny(item))
			}
			return out
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			if arrRaw, ok := obj["items"].([]any); ok {
				out := make([]map[string]string, 0, len(arrRaw))
				for _, item := range arrRaw {
					if m, ok := item.(map[string]any); ok {
						out = append(out, outlookCredentialFromAny(m))
					}
				}
				return out
			}
		}
	}
	lines := strings.Split(raw, "\n")
	out := make([]map[string]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "{") && strings.Contains(line, "}") {
			if cred := outlookCredentialFromLineObject(line); len(cred) > 0 {
				out = append(out, cred)
				continue
			}
		}
		if strings.Contains(line, "----") {
			fields := strings.SplitN(line, "----", 4)
			if len(fields) == 4 {
				cred := map[string]string{
					"email":         strings.TrimSpace(fields[0]),
					"password":      strings.TrimSpace(fields[1]),
					"client_id":     strings.TrimSpace(fields[2]),
					"refresh_token": strings.TrimSpace(fields[3]),
				}
				out = append(out, cred)
			}
			continue
		}
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == '\t' || r == ';' || r == '|'
		})
		if len(fields) == 0 {
			continue
		}
		cred := map[string]string{}
		if len(fields) > 0 {
			cred["email"] = strings.TrimSpace(fields[0])
		}
		if len(fields) > 1 {
			cred["refresh_token"] = strings.TrimSpace(fields[1])
		}
		if len(fields) > 2 {
			cred["client_id"] = strings.TrimSpace(fields[2])
		}
		if len(fields) > 3 {
			cred["password"] = strings.TrimSpace(fields[3])
		}
		if len(fields) > 4 {
			cred["name"] = strings.TrimSpace(fields[4])
		}
		out = append(out, cred)
	}
	return out
}

func outlookCredentialFromAny(item map[string]any) map[string]string {
	out := map[string]string{}
	if item == nil {
		return out
	}
	for _, key := range []string{"email", "refresh_token", "client_id", "password", "name", "mailbox_name"} {
		if v := strings.TrimSpace(strAny(item[key], "")); v != "" {
			if key == "mailbox_name" {
				out["name"] = v
				continue
			}
			out[key] = v
		}
	}
	return out
}

func outlookCredentialFromLineObject(line string) map[string]string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return nil
	}
	return outlookCredentialFromAny(obj)
}

func outlookCredentialAvailable(entry map[string]any) bool {
	if entry == nil {
		return true
	}
	state := strings.ToLower(strings.TrimSpace(strAny(entry["state"], "")))
	switch state {
	case "used", "failed", "token_invalid":
		return false
	case "in_use":
		updated := strings.TrimSpace(strAny(entry["updated_at"], ""))
		if updated == "" {
			return true
		}
		ts, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return true
		}
		return time.Since(ts) >= time.Hour
	default:
		return true
	}
}

func normalizeGraphItem(item map[string]any) map[string]any {
	body := map[string]any{}
	if m, ok := item["body"].(map[string]any); ok {
		body = m
	}
	contentType := strings.ToLower(strings.TrimSpace(strAny(body["contentType"], "")))
	content := strAny(body["content"], "")
	textContent := content
	htmlContent := ""
	if contentType == "html" {
		htmlContent = content
		textContent = strAny(item["bodyPreview"], "")
	}
	from := ""
	if sender, ok := item["from"].(map[string]any); ok {
		if addr, ok := sender["emailAddress"].(map[string]any); ok {
			from = strAny(addr["address"], strAny(addr["name"], ""))
		}
	}
	return map[string]any{
		"subject":      strAny(item["subject"], ""),
		"sender":       from,
		"text_content": textContent,
		"html_content": htmlContent,
		"received_at":  strAny(item["receivedDateTime"], ""),
		"raw":          item,
	}
}

func unwrapDataEnvelope(v any) any {
	if m, ok := v.(map[string]any); ok {
		if data, exists := m["data"]; exists {
			return data
		}
	}
	return v
}

func duckMailItems(data any) []map[string]any {
	data = unwrapDataEnvelope(data)
	if arr, ok := data.([]any); ok {
		return mapsFromAnySlice(arr)
	}
	if m, ok := data.(map[string]any); ok {
		for _, key := range []string{"hydra:member", "member", "data", "messages", "emails"} {
			if arr, ok := m[key].([]any); ok {
				return mapsFromAnySlice(arr)
			}
		}
	}
	return nil
}

func genericMailItems(data any) []map[string]any {
	data = unwrapDataEnvelope(data)
	if arr, ok := data.([]any); ok {
		return mapsFromAnySlice(arr)
	}
	if m, ok := data.(map[string]any); ok {
		for _, key := range []string{"emails", "messages", "items", "value", "results", "data"} {
			if arr, ok := m[key].([]any); ok {
				return mapsFromAnySlice(arr)
			}
		}
	}
	return nil
}

func mapsFromAnySlice(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func latestMailItem(items []map[string]any) map[string]any {
	if len(items) == 0 {
		return nil
	}
	best := items[0]
	bestTime := mailItemTimestamp(best)
	for _, item := range items[1:] {
		ts := mailItemTimestamp(item)
		if ts.After(bestTime) || (ts.Equal(bestTime) && strAny(item["id"], "") > strAny(best["id"], "")) {
			best = item
			bestTime = ts
		}
	}
	return best
}

func mailItemTimestamp(item map[string]any) time.Time {
	if item == nil {
		return time.Time{}
	}
	for _, key := range []string{"timestamp", "created_at", "createdAt", "received_at", "receivedAt", "date"} {
		raw := strings.TrimSpace(strAny(item[key], ""))
		if raw == "" {
			continue
		}
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			if f > 1000000000000 {
				return time.UnixMilli(int64(f))
			}
			if f > 1000000000 {
				return time.Unix(int64(f), 0)
			}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, raw); err == nil {
				return ts
			}
		}
	}
	return time.Time{}
}

func normalizeGenericMailMessage(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	textContent := firstNonEmpty(
		strAny(item["text_content"], ""),
		strAny(item["text"], ""),
		strAny(item["body"], ""),
		strAny(item["content"], ""),
		strAny(item["bodyPreview"], ""),
	)
	htmlContent := firstNonEmpty(
		strAny(item["html_content"], ""),
		strAny(item["html"], ""),
		strAny(item["html_body"], ""),
		strAny(item["body_html"], ""),
	)
	if htmlContent == "" {
		if body, ok := item["body"].(map[string]any); ok {
			content := strAny(body["content"], "")
			if strings.EqualFold(strAny(body["contentType"], ""), "html") {
				htmlContent = content
			} else if textContent == "" {
				textContent = content
			}
		}
	}
	if arr, ok := item["html"].([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, part := range arr {
			parts = append(parts, strAny(part, ""))
		}
		htmlContent = strings.Join(parts, "")
	}
	sender := firstNonEmpty(strAny(item["from_address"], ""), strAny(item["sender"], ""), strAny(item["from"], ""))
	if senderMap, ok := item["from"].(map[string]any); ok {
		sender = firstNonEmpty(strAny(senderMap["address"], ""), strAny(senderMap["email"], ""), strAny(senderMap["name"], ""), sender)
	}
	return map[string]any{
		"provider":     strAny(item["provider"], ""),
		"mailbox":      firstNonEmpty(strAny(item["mailbox"], ""), strAny(item["address"], ""), strAny(item["email"], "")),
		"message_id":   firstNonEmpty(strAny(item["message_id"], ""), strAny(item["id"], ""), strAny(item["_id"], ""), strAny(item["token"], "")),
		"subject":      strAny(item["subject"], ""),
		"sender":       sender,
		"text_content": textContent,
		"html_content": htmlContent,
		"received_at":  firstNonEmpty(strAny(item["created_at"], ""), strAny(item["createdAt"], ""), strAny(item["received_at"], ""), strAny(item["receivedAt"], ""), strAny(item["date"], ""), strAny(item["timestamp"], "")),
		"raw":          item,
	}
}

func normalizeInbucketMessage(summary, detail map[string]any) map[string]any {
	body := map[string]any{}
	if m, ok := detail["body"].(map[string]any); ok {
		body = m
	}
	header := map[string]any{}
	if m, ok := detail["header"].(map[string]any); ok {
		header = m
	}
	return map[string]any{
		"subject":      firstNonEmpty(strAny(detail["subject"], ""), strAny(summary["subject"], "")),
		"sender":       firstNonEmpty(strAny(detail["from"], ""), strAny(summary["from"], "")),
		"text_content": strAny(body["text"], ""),
		"html_content": strAny(body["html"], ""),
		"received_at":  firstNonEmpty(strAny(detail["date"], ""), strAny(summary["date"], "")),
		"to":           header["To"],
		"raw":          detail,
	}
}

func messageMatchesEmail(message map[string]any, address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return true
	}
	candidates := []string{
		strAny(message["mailbox"], ""),
		strAny(message["address"], ""),
		strAny(message["email"], ""),
		strAny(message["to"], ""),
		strAny(message["toEmail"], ""),
		strAny(message["mailTo"], ""),
		strAny(message["recipient"], ""),
		strAny(message["raw"], ""),
	}
	for _, key := range []string{"headers", "header"} {
		if m, ok := message[key].(map[string]any); ok {
			candidates = append(candidates, strAny(m["To"], ""), strAny(m["to"], ""))
		}
	}
	for _, candidate := range candidates {
		if strings.Contains(strings.ToLower(candidate), address) {
			return true
		}
	}
	return false
}

func messageBeforeCodeBoundary(mailbox *registerMailbox, message map[string]any) bool {
	if mailbox == nil || mailbox.CodeNotBefore.IsZero() || message == nil {
		return false
	}
	ts := mailItemTimestamp(message)
	if ts.IsZero() {
		if raw, ok := message["raw"].(map[string]any); ok {
			ts = mailItemTimestamp(raw)
		}
	}
	if ts.IsZero() {
		return false
	}
	return ts.Before(mailbox.CodeNotBefore)
}

func parseRawRecipient(rawText string) string {
	rawText = strings.TrimSpace(rawText)
	if rawText == "" {
		return ""
	}
	re := regexp.MustCompile(`(?im)^To:\s*(.+?)$`)
	matches := re.FindStringSubmatch(rawText)
	if len(matches) < 2 {
		return ""
	}
	value := strings.TrimSpace(matches[1])
	value = regexp.MustCompile(`\s*<[^>]*>`).ReplaceAllString(value, "")
	return strings.ToLower(strings.TrimSpace(value))
}

func isWrongEmailOTPCode(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "wrong_email_otp_code") || strings.Contains(text, "wrong code")
}

func firstErr(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}

func extractMessageItems(data map[string]any) []any {
	for _, key := range []string{"emails", "messages", "value", "results", "data"} {
		if items, ok := data[key].([]any); ok {
			return items
		}
	}
	return nil
}

var registerCodeStyledRegex = regexp.MustCompile(`(?is)background-color:\s*#F3F3F3[^>]*>[\s\S]*?(\d{6})[\s\S]*?</p>`)
var registerCodeContextRegex = regexp.MustCompile(`(?is)(?:verification\s+code\s+to\s+continue|输入此临时验证码以继续|临时验证码以继续)[\s\S]{0,500}?(\d{6})`)
var registerCodePhraseRegex = regexp.MustCompile(`(?i)(?:Verification code|code is|代码为|验证码)[:\s]*(\d{6})`)
var registerCodeFallbackRegex = regexp.MustCompile(`(?s)>\s*(\d{6})\s*<|\b(\d{6})\b`)

func extractRegisterCode(message any) string {
	msg, _ := message.(map[string]any)
	if msg == nil {
		return ""
	}
	content := html.UnescapeString(strings.Join([]string{
		strAny(msg["subject"], ""),
		strAny(msg["text_content"], ""),
		strAny(msg["html_content"], ""),
	}, "\n"))
	if content == "" {
		return ""
	}
	if match := registerCodeStyledRegex.FindStringSubmatch(content); len(match) > 1 && match[1] != "177010" {
		return match[1]
	}
	if match := registerCodeContextRegex.FindStringSubmatch(content); len(match) > 1 && match[1] != "177010" {
		return match[1]
	}
	if match := registerCodePhraseRegex.FindStringSubmatch(content); len(match) > 1 && match[1] != "177010" {
		return match[1]
	}
	matches := registerCodeFallbackRegex.FindAllStringSubmatchIndex(content, -1)
	for _, match := range matches {
		for i := 2; i+1 < len(match); i += 2 {
			start, end := match[i], match[i+1]
			if start < 0 || end < 0 {
				continue
			}
			value := content[start:end]
			if value == "" || value == "177010" {
				continue
			}
			if start > 0 {
				prev := content[start-1]
				if prev == '#' || prev == '&' {
					continue
				}
			}
			return value
		}
	}
	return ""
}

func registerMessageHasContent(message map[string]any) bool {
	if message == nil {
		return false
	}
	return strings.TrimSpace(strings.Join([]string{
		strAny(message["subject"], ""),
		strAny(message["sender"], ""),
		strAny(message["text_content"], ""),
		strAny(message["html_content"], ""),
	}, "\n")) != ""
}

func messageTrackingRef(message map[string]any) string {
	if message == nil {
		return ""
	}
	provider := strings.TrimSpace(strAny(message["provider"], ""))
	mailbox := strings.TrimSpace(strAny(message["mailbox"], ""))
	messageID := strings.TrimSpace(strAny(message["message_id"], ""))
	if messageID != "" {
		return "id:" + provider + ":" + mailbox + ":" + messageID
	}
	receivedAt := strings.TrimSpace(strAny(message["received_at"], ""))
	content := strings.Join([]string{
		strAny(message["subject"], ""),
		strAny(message["sender"], ""),
		strAny(message["text_content"], ""),
		strAny(message["html_content"], ""),
	}, "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("content:%s:%s:%s:%x", provider, mailbox, receivedAt, sum[:])
}

func mailboxSeenCodeRefs(mailbox *registerMailbox) map[string]bool {
	if mailbox == nil {
		return map[string]bool{}
	}
	if mailbox.Extra == nil {
		mailbox.Extra = map[string]any{}
	}
	if seen, ok := mailbox.Extra["_seen_code_message_refs"].(map[string]bool); ok {
		return seen
	}
	seen := map[string]bool{}
	if arr, ok := mailbox.Extra["_seen_code_message_refs"].([]string); ok {
		for _, item := range arr {
			if strings.TrimSpace(item) != "" {
				seen[item] = true
			}
		}
	}
	mailbox.Extra["_seen_code_message_refs"] = seen
	return seen
}

func mailboxRememberCodeRef(mailbox *registerMailbox, ref string) {
	ref = strings.TrimSpace(ref)
	if mailbox == nil || ref == "" {
		return
	}
	seen := mailboxSeenCodeRefs(mailbox)
	seen[ref] = true
}

func normalizeStringList(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s := strings.TrimSpace(strAny(item, "")); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if arr, ok := v.([]string); ok {
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	s := strings.TrimSpace(strAny(v, ""))
	if s == "" {
		return nil
	}
	return []string{s}
}

func randomSubdomainLabel() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
