package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/rand"
	"mime"
	stdhttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	http "github.com/bogdanfinn/fhttp"
)

const chatGPTBaseURL = "https://chatgpt.com"
const defaultClientVersion = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
const defaultClientBuild = "5955942"
const defaultPowScript = "https://chatgpt.com/backend-api/sentinel/sdk.js"
const bootstrapMaxAttempts = 3
const clientCreatedRoot = "client-created-root"

type UpstreamClient struct {
	token                    string
	proxyRoute               proxyRoute
	imageConversationStarted bool
	textConversationStarted  bool
	defaultModelSlug         string
	client                   upstreamHTTPDoer
	userAgent                string
	deviceID                 string
	sessionID                string
	secCHUA                  string
	secCHUAMobile            string
	secCHUAPlatform          string
	scriptSources            []string
	dataBuild                string
	solverURL                string
	solverKey                string
	localSolver              sentinelLocalSolver
	sentinelIdentity         sentinelIdentity
	timezone                 string
	browserDate              string
	imageTurnTrace           string
	imageBrowserID           string
}

type chatRequirements struct{ Token, ProofToken, TurnstileToken, SOToken string }

type upstreamImageResult struct {
	Bytes         []byte
	RevisedPrompt string
}

type upstreamStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("%s %s failed: status=%d body=%s", e.Method, e.Path, e.StatusCode, e.Body)
}

func retryAfterDuration(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(min(max(0, seconds), 86400)) * time.Second
	}
	if date, err := stdhttp.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}

type imageGenerationOptions struct {
	Timeout         time.Duration
	PollInterval    time.Duration
	PollInitialWait time.Duration
	UploadTimeout   time.Duration
}

func normalizeImageGenerationOptions(opts imageGenerationOptions) imageGenerationOptions {
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 4 * time.Second
	}
	if opts.PollInitialWait < 0 {
		opts.PollInitialWait = 0
	}
	if opts.UploadTimeout <= 0 {
		opts.UploadTimeout = opts.Timeout
	}
	return opts
}

type UpstreamTextEvent struct {
	Delta          string
	ConversationID string
	MessageID      string
	CurrentNode    string
	FileIDs        []string
	SedimentIDs    []string
	Blocked        bool
	ToolInvoked    any
	TurnUseCase    string
	AccountToken   string
}

func NewUpstreamClient(token, proxyURL string, binGetter func() (string, error)) (*UpstreamClient, error) {
	return NewUpstreamClientForAccount(Account{AccessToken: token}, proxyURL, binGetter)
}

func NewUpstreamClientForAccount(account Account, proxyURL string, binGetter func() (string, error), configs ...Config) (*UpstreamClient, error) {
	token := account.AccessToken
	fp := accountFingerprint(account)
	client, err := newUpstreamHTTPClient(proxyURL, binGetter)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	solverURL := firstNonEmpty(os.Getenv("CHATGPT2API_SENTINEL_SOLVER_URL"), cfg.SentinelSolverURL)
	solverKey := firstNonEmpty(os.Getenv("CHATGPT2API_SENTINEL_SOLVER_KEY"), cfg.SentinelSolverKey)
	upstream := &UpstreamClient{token: token, defaultModelSlug: ptrString(account.DefaultModelSlug), client: client, userAgent: fp["user-agent"], deviceID: fp["oai-device-id"], sessionID: fp["oai-session-id"], secCHUA: fp["sec-ch-ua"], secCHUAMobile: fp["sec-ch-ua-mobile"], secCHUAPlatform: fp["sec-ch-ua-platform"], scriptSources: []string{defaultPowScript}, solverURL: strings.TrimRight(strings.TrimSpace(solverURL), "/"), solverKey: strings.TrimSpace(solverKey)}
	if upstream.solverURL == "" {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("CHATGPT2API_SENTINEL_MODE"))) {
		case "browser":
			upstream.localSolver = sharedSentinelSolver(proxyURL, upstream.userAgent)
		case "node":
			upstream.localSolver = &nodeSentinelSolver{client: upstream}
		default:
			upstream.localSolver = &gojaSentinelSolver{client: upstream}
		}
	}
	return upstream, nil
}

func accountFingerprint(account Account) map[string]string {
	fp := map[string]string{}
	for k, v := range account.FP {
		if strings.TrimSpace(v) != "" {
			fp[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	defaults := map[string]string{
		"user-agent":         upstreamUserAgent(),
		"impersonate":        "edge_101",
		"oai-device-id":      uuid4(),
		"oai-session-id":     uuid4(),
		"sec-ch-ua":          upstreamSecCHUA(),
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}
	for k, v := range defaults {
		if strings.TrimSpace(fp[k]) == "" {
			fp[k] = v
		}
	}
	return fp
}

func (c *UpstreamClient) headers(path string, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", c.userAgent)
	h.Set("Origin", chatGPTBaseURL)
	h.Set("Referer", chatGPTBaseURL+"/")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")
	h.Set("Sec-Ch-Ua", c.secCHUA)
	h.Set("Sec-Ch-Ua-Arch", `"x86"`)
	h.Set("Sec-Ch-Ua-Bitness", `"64"`)
	h.Set("Sec-Ch-Ua-Full-Version", `"146.0.0.0"`)
	h.Set("Sec-Ch-Ua-Full-Version-List", `"Chromium";v="146.0.0.0", "Google Chrome";v="146.0.0.0", "Not A(Brand";v="99.0.0.0"`)
	h.Set("Sec-Ch-Ua-Mobile", c.secCHUAMobile)
	h.Set("Sec-Ch-Ua-Model", `""`)
	h.Set("Sec-Ch-Ua-Platform", c.secCHUAPlatform)
	h.Set("Sec-Ch-Ua-Platform-Version", `"19.0.0"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("OAI-Did", c.deviceID)
	h.Set("OAI-Session-Id", c.sessionID)
	h.Set("OAI-Language", "zh-CN")
	h.Set("OAI-Client-Version", defaultClientVersion)
	h.Set("OAI-Client-Build-Number", defaultClientBuild)
	h.Set("X-OpenAI-Target-Route", path)
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
		if accountID := chatGPTAccountID(c.token); accountID != "" {
			h.Set("ChatGPT-Account-Id", accountID)
		}
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	h[http.HeaderOrderKey] = []string{
		"user-agent", "accept", "content-type", "authorization", "chatgpt-account-id", "origin", "referer", "accept-language",
		"cache-control", "pragma", "priority", "sec-ch-ua", "sec-ch-ua-arch", "sec-ch-ua-bitness",
		"sec-ch-ua-full-version", "sec-ch-ua-full-version-list", "sec-ch-ua-mobile", "sec-ch-ua-model",
		"sec-ch-ua-platform", "sec-ch-ua-platform-version", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		"oai-did", "oai-session-id",
		"oai-language", "oai-client-version", "oai-client-build-number",
		"x-openai-target-route", "openai-sentinel-chat-requirements-token", "openai-sentinel-proof-token",
		"openai-sentinel-turnstile-token", "openai-sentinel-so-token", "x-conduit-token", "x-oai-turn-trace-id",
	}
	return h
}
func (c *UpstreamClient) do(ctx context.Context, method, path string, headers map[string]string, body any, stream bool) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	traceLogf(ctx, "│  ├─ upstream request %s %s stream=%v", method, path, stream)
	traceLogf(ctx, "│  │  headers: %s", traceHeaderPreview(headers))
	traceLogf(ctx, "│  │  body: %s", traceBodyPreview(body))
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, chatGPTBaseURL+path, rdr)
	if err != nil {
		traceLogf(ctx, "│  └─ upstream request build failed duration=%s error=%v", traceHTTPDuration(start), err)
		return nil, err
	}
	req.Header = c.headers(strings.Split(path, "?")[0], headers)
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		traceLogf(ctx, "│  └─ upstream network failed duration=%s error=%v", traceHTTPDuration(start), err)
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		traceLogf(ctx, "│  └─ upstream response status=%d duration=%s error_body=%s", resp.StatusCode, traceHTTPDuration(start), truncateText(string(b), 800))
		return nil, &upstreamStatusError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(b), RetryAfter: retryAfterDuration(resp.Header.Get("Retry-After"), time.Now())}
	}
	traceLogf(ctx, "│  └─ upstream response status=%d duration=%s", resp.StatusCode, traceHTTPDuration(start))
	if !stream {
		return resp, nil
	}
	return resp, nil
}
func (c *UpstreamClient) bootstrapHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", c.userAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Sec-Ch-Ua", c.secCHUA)
	h.Set("Sec-Ch-Ua-Mobile", c.secCHUAMobile)
	h.Set("Sec-Ch-Ua-Platform", c.secCHUAPlatform)
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Upgrade-Insecure-Requests", "1")
	h[http.HeaderOrderKey] = []string{
		"user-agent", "accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
		"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		"sec-fetch-user", "upgrade-insecure-requests",
	}
	return h
}
func (c *UpstreamClient) bootstrap(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= bootstrapMaxAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt-1) * 300 * time.Millisecond
			traceLogf(ctx, "│  ├─ bootstrap retry attempt=%d delay=%s previous_error=%v", attempt, delay, lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err := c.bootstrapOnce(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if isProxyConnectionFailure(err) {
			return err
		}
		if !isRetryableBootstrapError(err) {
			return err
		}
	}
	return lastErr
}

func (c *UpstreamClient) bootstrapOnce(ctx context.Context) error {
	traceLogf(ctx, "│  ├─ step bootstrap ChatGPT home page")
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, chatGPTBaseURL+"/", nil)
	req.Header = c.bootstrapHeaders()
	traceLogf(ctx, "│  │  request GET / headers: %s", traceHeaderPreview(req.Header))
	resp, err := c.client.Do(req)
	if err != nil {
		traceLogf(ctx, "│  └─ bootstrap failed duration=%s error=%v", traceHTTPDuration(start), err)
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusProxyAuthRequired {
			traceLogf(ctx, "│  └─ bootstrap proxy authentication failed status=407 duration=%s", traceHTTPDuration(start))
			return &proxyConnectionError{cause: errors.New("bootstrap proxy authentication failed: HTTP 407")}
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		location := strings.TrimSpace(resp.Header.Get("Location"))
		traceLogf(ctx, "│  └─ bootstrap response status=%d location=%q duration=%s body=%s", resp.StatusCode, location, traceHTTPDuration(start), truncateText(string(b), 800))
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return fmt.Errorf("bootstrap redirect: status=%d location=%s body=%s", resp.StatusCode, location, string(b))
		}
		return fmt.Errorf("bootstrap failed: status=%d body=%s", resp.StatusCode, string(b))
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	c.scriptSources, c.dataBuild = parsePowResources(string(b))
	if len(c.scriptSources) == 0 {
		c.scriptSources = []string{defaultPowScript}
	}
	traceLogf(ctx, "│  └─ bootstrap ok status=%d duration=%s pow_scripts=%d data_build=%q", resp.StatusCode, traceHTTPDuration(start), len(c.scriptSources), c.dataBuild)
	return nil
}

func isRetryableBootstrapError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, "bootstrap") {
		return false
	}
	if strings.Contains(text, "status=401") || strings.Contains(text, "status=403") {
		return false
	}
	if strings.Contains(text, "redirect") || strings.Contains(text, "status=3") || strings.Contains(text, "status=429") {
		return true
	}
	for _, code := range []string{"status=500", "status=502", "status=503", "status=504"} {
		if strings.Contains(text, code) {
			return true
		}
	}
	return !strings.Contains(text, "status=")
}
func (c *UpstreamClient) chatRequirements(ctx context.Context) (chatRequirements, error) {
	traceLogf(ctx, "│  ├─ step build sentinel chat requirements")
	base := "/backend-api/sentinel/chat-requirements"
	if c.token == "" {
		base = "/backend-anon/sentinel/chat-requirements"
	}
	ua, err := c.sentinelUserAgent(ctx)
	if err != nil {
		return chatRequirements{}, err
	}
	p := buildLegacyRequirementsToken(ua, c.scriptSources, c.dataBuild, c.powDate(), c.sentinelIdentity)
	resp, err := c.do(ctx, http.MethodPost, base+"/prepare", map[string]string{"Content-Type": "application/json"}, map[string]any{"p": p}, false)
	if err != nil {
		return chatRequirements{}, err
	}
	var prepared map[string]any
	err = json.NewDecoder(resp.Body).Decode(&prepared)
	resp.Body.Close()
	if err != nil {
		return chatRequirements{}, err
	}
	if ark, ok := prepared["arkose"].(map[string]any); ok && boolAny(ark["required"], false) {
		return chatRequirements{}, errors.New("upstream requires arkose token; not implemented")
	}
	prepareToken := strAny(prepared["prepare_token"], "")
	if prepareToken == "" {
		return chatRequirements{}, errors.New("missing sentinel prepare token")
	}
	cr := chatRequirements{}
	if pow, ok := prepared["proofofwork"].(map[string]any); ok && boolAny(pow["required"], false) {
		proof, err := buildProofToken(strAny(pow["seed"], ""), strAny(pow["difficulty"], ""), ua, c.scriptSources, c.dataBuild, c.powDate(), c.sentinelIdentity)
		if err != nil {
			return cr, err
		}
		cr.ProofToken = proof
	}
	if turnstile, ok := prepared["turnstile"].(map[string]any); ok && boolAny(turnstile["required"], false) {
		cr.TurnstileToken, err = c.solveSentinelTurnstile(ctx, strAny(turnstile["dx"], ""), p, prepared)
		if err != nil {
			return cr, err
		}
	}
	resp, err = c.do(ctx, http.MethodPost, base+"/finalize", map[string]string{"Content-Type": "application/json"}, map[string]any{"prepare_token": prepareToken, "proofofwork": cr.ProofToken, "turnstile": cr.TurnstileToken}, false)
	if err != nil {
		return cr, err
	}
	defer resp.Body.Close()
	var finalized map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&finalized); err != nil {
		return cr, err
	}
	cr.Token = strAny(finalized["token"], "")
	if cr.Token == "" {
		traceLogf(ctx, "│  └─ sentinel failed missing requirements token")
		return cr, errors.New("missing chat requirements token")
	}
	traceLogf(ctx, "│  └─ sentinel ok token=%v proof=%v turnstile=%v so=%v", cr.Token != "", cr.ProofToken != "", cr.TurnstileToken != "", cr.SOToken != "")
	return cr, nil
}
func (c *UpstreamClient) conversationHeaders(path string, cr chatRequirements) map[string]string {
	h := map[string]string{"Accept": "text/event-stream", "Content-Type": "application/json", "OpenAI-Sentinel-Chat-Requirements-Token": cr.Token}
	if cr.ProofToken != "" {
		h["OpenAI-Sentinel-Proof-Token"] = cr.ProofToken
	}
	if cr.TurnstileToken != "" {
		h["OpenAI-Sentinel-Turnstile-Token"] = cr.TurnstileToken
	}
	if cr.SOToken != "" {
		h["OpenAI-Sentinel-SO-Token"] = cr.SOToken
	}
	return h
}
func (c *UpstreamClient) imageHeaders(path string, cr chatRequirements, conduit, accept string) map[string]string {
	h := map[string]string{"Content-Type": "application/json", "Accept": accept, "OpenAI-Sentinel-Chat-Requirements-Token": cr.Token, "Originator": "Codex Browser", "X-OpenAI-Codex-Window-Type": "browser", "X-OpenAI-Web-Frontend": "codex_webview", "X-Oai-Mcp-Form-Version": "2"}
	if cr.ProofToken != "" {
		h["OpenAI-Sentinel-Proof-Token"] = cr.ProofToken
	}
	if cr.TurnstileToken != "" {
		h["OpenAI-Sentinel-Turnstile-Token"] = cr.TurnstileToken
	}
	if cr.SOToken != "" {
		h["OpenAI-Sentinel-SO-Token"] = cr.SOToken
	}
	if conduit != "" {
		h["X-Conduit-Token"] = conduit
	}
	if c.imageTurnTrace != "" {
		h["X-Oai-Turn-Trace-Id"] = c.imageTurnTrace
	} else if accept == "text/event-stream" {
		h["X-Oai-Turn-Trace-Id"] = uuid4()
	}
	return h
}

func (c *UpstreamClient) StreamText(ctx context.Context, messages []map[string]any, model string) (<-chan string, <-chan error) {
	return c.StreamTextConversation(ctx, messages, model, "")
}

func (c *UpstreamClient) StreamTextConversation(ctx context.Context, messages []map[string]any, model, conversationID string) (<-chan string, <-chan error) {
	events, errs := c.StreamTextConversationEvents(ctx, messages, model, conversationID)
	out := make(chan string)
	go func() {
		defer close(out)
		for ev := range events {
			if ev.Delta != "" {
				out <- ev.Delta
			}
		}
	}()
	return out, errs
}

func (c *UpstreamClient) StreamTextConversationEvents(ctx context.Context, messages []map[string]any, model, conversationID string) (<-chan UpstreamTextEvent, <-chan error) {
	return c.streamTextConversationEvents(ctx, messages, model, conversationID, true)
}

func (c *UpstreamClient) StreamChatConversationEvents(ctx context.Context, messages []map[string]any, model, conversationID string) (<-chan UpstreamTextEvent, <-chan error) {
	return c.streamTextConversationEvents(ctx, messages, model, conversationID, false)
}

func (c *UpstreamClient) streamTextConversationEvents(ctx context.Context, messages []map[string]any, model, conversationID string, historyDisabled bool) (<-chan UpstreamTextEvent, <-chan error) {
	c.textConversationStarted = false
	out := make(chan UpstreamTextEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		traceLogf(ctx, "├─ text stream start model=%s history_disabled=%v conversation_id=%s messages=%d", model, historyDisabled, maskedValue(conversationID), len(messages))
		if err := c.bootstrap(ctx); err != nil {
			errs <- err
			return
		}
		cr, err := c.chatRequirements(ctx)
		if err != nil {
			errs <- err
			return
		}
		if model == "" {
			model = "auto"
		}
		modelOpts := normalizeTextModelOptions(model)
		timezone, timezoneOffset, _ := c.timezoneInfo()
		parentID := uuid4()
		conversationID = strings.TrimSpace(conversationID)
		if conversationID != "" {
			if conv, err := c.getConversation(ctx, conversationID); err == nil {
				if cur := strings.TrimSpace(strAny(conv["current_node"], "")); cur != "" {
					parentID = cur
				}
			}
		}
		traceLogf(ctx, "│  ├─ step convert messages to ChatGPT conversation format")
		conversationMessages, err := c.toConversationMessages(ctx, messages)
		if err != nil {
			errs <- err
			return
		}
		payload := map[string]any{"action": "next", "messages": conversationMessages, "model": modelOpts.Model, "parent_message_id": parentID, "conversation_mode": map[string]any{"kind": "primary_assistant"}, "conversation_origin": nil, "force_paragen": false, "force_paragen_model_slug": "", "force_rate_limit": false, "force_use_sse": true, "history_and_training_disabled": historyDisabled, "reset_rate_limits": false, "suggestions": []any{}, "supported_encodings": []string{}, "system_hints": []string{}, "timezone": timezone, "timezone_offset_min": timezoneOffset, "variant_purpose": "comparison_implicit", "websocket_request_id": uuid4(), "client_contextual_info": clientContext()}
		if modelOpts.ReasoningLevel != "" {
			payload["thinking_effort"] = modelOpts.ReasoningLevel
		}
		if conversationID != "" {
			payload["conversation_id"] = conversationID
			payload["history_and_training_disabled"] = false
		}
		path := "/backend-api/conversation"
		if c.token == "" {
			path = "/backend-anon/conversation"
		}
		traceLogf(ctx, "│  ├─ step open upstream SSE conversation")
		c.textConversationStarted = true
		resp, err := c.do(ctx, http.MethodPost, path, c.conversationHeaders(path, cr), payload, true)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		historyMessages := assistantHistoryMessages(messages)
		historyText := strings.Join(historyMessages, "")
		state := newUpstreamConversationState(historyText, historyMessages)
		eventCount := 0
		for ev := range parseSSE(resp.Body) {
			if ev == "[DONE]" {
				traceLogf(ctx, "└─ text stream done events=%d", eventCount)
				return
			}
			eventCount++
			if meta, ok := state.Apply(ev); ok {
				meta.AccountToken = c.token
				out <- meta
			}
		}
	}()
	return out, errs
}

func (c *UpstreamClient) GenerateImage(ctx context.Context, prompt, model, size, resolution string, refs [][]byte, opts imageGenerationOptions) ([]upstreamImageResult, error) {
	opts = normalizeImageGenerationOptions(opts)
	c.imageConversationStarted = false
	traceLogf(ctx, "├─ image generation start model=%s size=%s resolution=%s refs=%d timeout=%s poll_interval=%s initial_wait=%s", model, size, resolution, len(refs), opts.Timeout, opts.PollInterval, opts.PollInitialWait)
	if c.token == "" {
		return nil, errors.New("access token is required for image generation")
	}
	if isCodexImageRequest(model, resolution) {
		traceLogf(ctx, "│  ├─ route to Codex image path")
		return c.GenerateCodexImage(ctx, buildImagePrompt(prompt, size), model, codexImageSize(size, resolution), refs)
	}
	finalPrompt := buildImagePromptWithOptions(prompt, size, resolution)
	if err := c.bootstrap(ctx); err != nil {
		return nil, err
	}
	cr, err := c.chatRequirements(ctx)
	if err != nil {
		return nil, err
	}
	uploads := []map[string]any{}
	for i, b := range refs {
		traceLogf(ctx, "│  ├─ step upload reference image %d/%d bytes=%d", i+1, len(refs), len(b))
		up, err := c.uploadImage(ctx, b, fmt.Sprintf("image_%d.png", i+1), opts.UploadTimeout)
		if err != nil {
			return nil, err
		}
		uploads = append(uploads, up)
	}
	traceLogf(ctx, "│  ├─ step prepare image conversation")
	conduit, err := c.prepareImage(ctx, finalPrompt, model, cr)
	if err != nil {
		return nil, err
	}
	traceLogf(ctx, "│  ├─ step start image SSE conversation uploads=%d", len(uploads))
	c.imageConversationStarted = true
	resp, err := c.startImage(ctx, finalPrompt, model, cr, conduit, uploads)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	cid := ""
	fileIDs := []string{}
	sedimentIDs := []string{}
	message := ""
	state := newUpstreamConversationState("", nil)
	generated := false
	// 限制 SSE 读取时长，防止上游无限挂起。
	sseCtx, sseCancel := context.WithTimeout(ctx, opts.Timeout)
	defer sseCancel()
	// 从 resp.Body 读取 SSE，受 sseCtx 控制
	done := make(chan struct{}, 1)
	go func() {
		defer func() { done <- struct{}{} }()
		for ev := range parseSSE(resp.Body) {
			if ev == "[DONE]" {
				generated = true
				return
			}
			updateImageState(ev, &cid, &message)
			if meta, ok := state.Apply(ev); ok {
				if meta.ConversationID != "" {
					cid = meta.ConversationID
				}
				fileIDs = unique(append(fileIDs, meta.FileIDs...))
				sedimentIDs = unique(append(sedimentIDs, meta.SedimentIDs...))
				if meta.Delta != "" || state.Text != "" {
					message = state.Text
				}
			}
		}
		generated = true
	}()
	select {
	case <-done:
	case <-sseCtx.Done():
		if !generated {
			return nil, fmt.Errorf("image generation SSE timed out (%ds)", int(opts.Timeout/time.Second))
		}
	}
	traceLogf(ctx, "│  ├─ image SSE result blocked=%t tool_invoked=%v turn_use_case=%q file_ids=%d sediment_ids=%d text_len=%d", state.Blocked, state.ToolInvoked, state.TurnUseCase, len(fileIDs), len(sedimentIDs), len(message))
	fileIDs, sedimentIDs, err = c.imageIDsAfterSSE(ctx, cid, message, state, fileIDs, sedimentIDs, opts)
	if err != nil {
		return nil, err
	}
	traceLogf(ctx, "│  ├─ step resolve image download URLs file_ids=%d sediment_ids=%d", len(fileIDs), len(sedimentIDs))
	urls, err := c.resolveImageURLs(ctx, cid, fileIDs, sedimentIDs)
	if err != nil {
		return nil, err
	}
	if len(urls) == 0 {
		if message != "" {
			return nil, imageTextReplyFailure(message)
		}
		return nil, errNoImageGenerated
	}
	results := []upstreamImageResult{}
	seenBytes := map[string]bool{}
	for i, u := range urls {
		traceLogf(ctx, "│  ├─ step download generated image %d/%d", i+1, len(urls))
		b, err := c.download(ctx, u)
		if err != nil {
			return nil, imageDownloadFailure(u, err)
		}
		sum := fmt.Sprintf("%x", md5.Sum(b))
		if seenBytes[sum] {
			traceLogf(ctx, "│  │  skip duplicate generated image %d", i+1)
			continue
		}
		seenBytes[sum] = true
		results = append(results, upstreamImageResult{Bytes: b, RevisedPrompt: prompt})
	}
	traceLogf(ctx, "└─ image generation done images=%d", len(results))
	return results, nil
}

func (c *UpstreamClient) imageIDsAfterSSE(ctx context.Context, cid, message string, state *upstreamConversationState, fileIDs, sedimentIDs []string, opts imageGenerationOptions) ([]string, []string, error) {
	if cid != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && !state.Blocked && !isImageQuotaMessage(message) {
		traceLogf(ctx, "│  ├─ step poll final image tool records conversation=%s timeout=%s interval=%s", maskedValue(cid), opts.Timeout, opts.PollInterval)
		f, s, err := c.pollImageIDsWithError(ctx, cid, opts.Timeout, opts.PollInterval, opts.PollInitialWait)
		if err != nil {
			return nil, nil, err
		}
		fileIDs = append(fileIDs, f...)
		sedimentIDs = append(sedimentIDs, s...)
	}
	if message != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && (state.Blocked || state.ToolInvoked == false || state.TurnUseCase == "text") {
		return nil, nil, imageTextReplyFailure(message)
	}
	return fileIDs, sedimentIDs, nil
}
func (c *UpstreamClient) prepareImage(ctx context.Context, prompt, model string, cr chatRequirements) (string, error) {
	path := "/backend-api/f/conversation/prepare"
	c.imageTurnTrace = uuid4()
	c.imageBrowserID = uuid4()
	timezone, timezoneOffset, _ := c.timezoneInfo()
	payload := map[string]any{"action": "next", "browser_context": map[string]any{"instance_id": c.imageBrowserID}, "parent_message_id": clientCreatedRoot, "model": c.imageModelSlug(model), "client_prepare_state": "sent", "client_prepare_dispatch": "debounced", "client_prepare_source": "composer_editor_state", "is_do_not_remember": false, "timezone_offset_min": timezoneOffset, "timezone": timezone, "thinking_effort": "standard", "system_hints": []string{"picture_v2"}, "partial_query": map[string]any{"id": uuid4(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}}, "supported_encodings": []string{"v1"}, "local_function_names": []string{"local.continue_in_work"}, "client_contextual_info": imageClientContext()}
	resp, err := c.do(ctx, http.MethodPost, path, c.imageHeaders(path, cr, "", "*/*"), payload, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	token := strAny(data["conduit_token"], "")
	if token == "" {
		return "", errors.New("missing conduit_token")
	}
	return token, nil
}
func (c *UpstreamClient) startImage(ctx context.Context, prompt, model string, cr chatRequirements, conduit string, refs []map[string]any) (*http.Response, error) {
	path := "/backend-api/f/conversation"
	timezone, timezoneOffset, _ := c.timezoneInfo()
	parts := []any{}
	attachments := []map[string]any{}
	for _, r := range refs {
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + strAny(r["file_id"], ""), "width": r["width"], "height": r["height"], "size_bytes": r["file_size"]})
		attachments = append(attachments, map[string]any{"id": r["file_id"], "mimeType": r["mime_type"], "name": r["file_name"], "size": r["file_size"], "width": r["width"], "height": r["height"]})
	}
	parts = append(parts, prompt)
	content := map[string]any{"content_type": "text", "parts": []string{prompt}}
	if len(refs) > 0 {
		content = map[string]any{"content_type": "multimodal_text", "parts": parts}
	}
	metadata := map[string]any{"developer_mode_connector_ids": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false, "system_hints": []string{"picture_v2"}, "serialization_metadata": map[string]any{"render_format": "markdown", "custom_symbol_offsets": []any{}}}
	if len(attachments) > 0 {
		metadata["attachments"] = attachments
	}
	payload := map[string]any{"action": "next", "browser_context": map[string]any{"instance_id": c.imageBrowserID}, "messages": []any{map[string]any{"id": uuid4(), "author": map[string]any{"role": "user", "name": nil, "metadata": map[string]any{}}, "channel": nil, "create_time": float64(time.Now().UnixNano()) / 1e9, "end_turn": nil, "content": content, "metadata": metadata, "recipient": "all", "status": "finished_successfully", "update_time": nil, "weight": 1}}, "parent_message_id": clientCreatedRoot, "model": c.imageModelSlug(model), "client_prepare_state": "success", "is_do_not_remember": false, "turn_attribution": map[string]any{"turn_trigger": "composer"}, "genui_state_snapshots": []any{}, "timezone_offset_min": timezoneOffset, "timezone": timezone, "thinking_effort": "standard", "system_hints": []string{"picture_v2"}, "supported_encodings": []string{"v1"}, "local_function_names": []string{"local.continue_in_work"}, "client_contextual_info": imageClientContext()}
	return c.do(ctx, http.MethodPost, path, c.imageHeaders(path, cr, conduit, "text/event-stream"), payload, true)
}

func imageClientContext() map[string]any {
	return map[string]any{"app_name": "chatgpt.com", "app_surface": "codex_browser", "has_web_push_capabilities": true, "web_push_notification_permission": "default"}
}
func (c *UpstreamClient) uploadImage(ctx context.Context, data []byte, name string, timeout time.Duration) (map[string]any, error) {
	traceLogf(ctx, "│  ├─ upload image begin name=%s bytes=%d", name, len(data))
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		cfg.Width = 1024
		cfg.Height = 1024
	}
	mimeType := stdhttp.DetectContentType(data)
	if ext := mime.TypeByExtension(filepath.Ext(name)); ext != "" {
		mimeType = ext
	}
	path := "/backend-api/files"
	meta := map[string]any{"file_name": name, "file_size": len(data), "use_case": "multimodal", "width": cfg.Width, "height": cfg.Height}
	traceLogf(ctx, "│  │  image metadata width=%d height=%d mime=%s", cfg.Width, cfg.Height, mimeType)
	resp, err := c.do(ctx, http.MethodPost, path, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, meta, false)
	if err != nil {
		return nil, err
	}
	var up map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&up)
	resp.Body.Close()
	uploadURL := strAny(up["upload_url"], "")
	fileID := strAny(up["file_id"], "")
	if uploadURL == "" || fileID == "" {
		return nil, errors.New("file upload metadata missing")
	}
	putReq, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	putReq.ContentLength = int64(len(data))
	putReq.Header.Set("Content-Type", mimeType)
	putReq.Header.Set("x-ms-blob-type", "BlockBlob")
	putReq.Header.Set("x-ms-version", "2020-04-08")
	putReq.Header.Set("Origin", chatGPTBaseURL)
	putReq.Header.Set("Referer", chatGPTBaseURL+"/")
	putReq.Header.Set("User-Agent", c.userAgent)
	putReq.Header.Set("Accept", "application/json, text/plain, */*")
	putReq.Header.Set("Accept-Language", "en-US,en;q=0.8")
	traceLogf(ctx, "│  ├─ object storage PUT %s bytes=%d headers: %s", safeURLForLog(uploadURL), len(data), traceHeaderPreview(putReq.Header))
	putStart := time.Now()
	put, err := (&stdhttp.Client{Timeout: timeout}).Do(putReq)
	if err != nil {
		traceLogf(ctx, "│  └─ object storage PUT failed duration=%s error=%v", traceHTTPDuration(putStart), err)
		return nil, err
	}
	putBody, _ := io.ReadAll(io.LimitReader(put.Body, 2048))
	put.Body.Close()
	if put.StatusCode < 200 || put.StatusCode >= 300 {
		traceLogf(ctx, "│  └─ object storage PUT response status=%d duration=%s body=%s", put.StatusCode, traceHTTPDuration(putStart), truncateText(strings.TrimSpace(string(putBody)), 500))
		return nil, fmt.Errorf("image upload failed: status=%d body=%s", put.StatusCode, strings.TrimSpace(string(putBody)))
	}
	traceLogf(ctx, "│  └─ object storage PUT ok status=%d duration=%s", put.StatusCode, traceHTTPDuration(putStart))
	done := fmt.Sprintf("/backend-api/files/%s/uploaded", fileID)
	resp, err = c.do(ctx, http.MethodPost, done, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, map[string]any{}, false)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	traceLogf(ctx, "│  └─ upload image done file_id=%s", maskedValue(fileID))
	return map[string]any{"file_id": fileID, "file_name": name, "file_size": len(data), "mime_type": mimeType, "width": cfg.Width, "height": cfg.Height}, nil
}
func (c *UpstreamClient) pollImageIDs(ctx context.Context, cid string, timeout, interval, initialWait time.Duration) ([]string, []string) {
	files, sediments, _ := c.pollImageIDsWithError(ctx, cid, timeout, interval, initialWait)
	return files, sediments
}

func (c *UpstreamClient) pollImageIDsWithError(ctx context.Context, cid string, timeout, interval, initialWait time.Duration) ([]string, []string, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if interval <= 0 {
		interval = 4 * time.Second
	}
	deadline := time.Now().Add(timeout)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if initialWait > 0 {
		initialWait = min(initialWait, time.Until(deadline))
		traceLogf(ctx, "│  │  image poll initial wait %s", initialWait)
		select {
		case <-pollCtx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(initialWait):
		}
	}
	consecutiveErrors := 0
	for time.Now().Before(deadline) {
		conv, err := c.getConversation(pollCtx, cid)
		wait := interval
		if err == nil {
			consecutiveErrors = 0
			f, s := extractToolIDs(conv)
			traceLogf(ctx, "│  │  image poll extracted file_ids=%d sediment_ids=%d", len(f), len(s))
			if len(f) > 0 || len(s) > 0 {
				return f, s, nil
			}
			if quotaMessage := imagePollQuotaMessage(conv); quotaMessage != "" {
				return nil, nil, errors.New(cleanImageMessage(quotaMessage))
			}
		} else {
			traceLogf(ctx, "│  │  image poll conversation failed: %v", err)
			var statusErr *upstreamStatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode != 404 && statusErr.StatusCode != 429 && (statusErr.StatusCode < 500 || statusErr.StatusCode > 599) {
				return nil, nil, err
			}
			consecutiveErrors++
			wait = imagePollRetryDelay(interval, consecutiveErrors, err)
			jitterMax := min(wait/10, 500*time.Millisecond)
			if jitterMax > 0 {
				wait += time.Duration(rand.Int63n(int64(jitterMax)))
			}
			traceLogf(ctx, "│  │  image poll retry after %s", wait)
			if pollCtx.Err() != nil {
				return nil, nil, ctx.Err()
			}
		}
		remaining := time.Until(deadline)
		if remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			break
		}
		select {
		case <-pollCtx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	traceLogf(ctx, "│  └─ image poll timed out conversation=%s timeout=%s", maskedValue(cid), timeout)
	return nil, nil, nil
}

func imagePollRetryDelay(interval time.Duration, failures int, err error) time.Duration {
	if interval <= 0 {
		interval = 4 * time.Second
	}
	delay := interval
	for i := 1; i < failures && delay < 16*time.Second; i++ {
		delay = min(delay*2, 16*time.Second)
	}
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) {
		delay = max(delay, statusErr.RetryAfter)
	}
	return delay
}

func (c *UpstreamClient) getConversation(ctx context.Context, cid string) (map[string]any, error) {
	path := "/backend-api/conversation/" + cid
	resp, err := c.do(ctx, http.MethodGet, path, map[string]string{"Accept": "application/json"}, nil, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]any
	err = json.NewDecoder(resp.Body).Decode(&data)
	return data, err
}
func (c *UpstreamClient) resolveImageURLs(ctx context.Context, cid string, fileIDs, sedimentIDs []string) ([]string, error) {
	urls := []string{}
	seen := map[string]bool{}
	var firstErr error
	for _, id := range unique(fileIDs) {
		if id == "" || id == "file_upload" {
			continue
		}
		path := "/backend-api/files/" + id + "/download"
		u, err := c.imageDownloadURL(ctx, path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if u != "" && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	if cid != "" {
		for _, id := range unique(sedimentIDs) {
			path := "/backend-api/conversation/" + cid + "/attachment/" + id + "/download"
			u, err := c.imageDownloadURL(ctx, path)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if u != "" && !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	if len(urls) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return urls, nil
}

func (c *UpstreamClient) imageDownloadURL(ctx context.Context, path string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.do(ctx, http.MethodGet, path, map[string]string{"Accept": "application/json"}, nil, false)
		if err == nil {
			var data map[string]any
			err = json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			if err == nil {
				if u := strings.TrimSpace(strAny(data["download_url"], strAny(data["url"], ""))); u != "" {
					return u, nil
				}
				err = fmt.Errorf("%s returned no download URL", path)
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var statusErr *upstreamStatusError
		if attempt == 1 || (errors.As(err, &statusErr) && statusErr.StatusCode != 404 && statusErr.StatusCode != 429 && (statusErr.StatusCode < 500 || statusErr.StatusCode > 599)) {
			return "", err
		}
		traceLogf(ctx, "│  │  image download URL retry path=%s error=%v", path, err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", errors.New("image download URL unavailable")
}

type imageDownloadStatusError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *imageDownloadStatusError) Error() string {
	return fmt.Sprintf("download failed: status=%d body=%.100s", e.StatusCode, e.Body)
}

func (c *UpstreamClient) download(ctx context.Context, u string) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		data, err := c.downloadOnce(ctx, u)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var statusErr *imageDownloadStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode != 429 && (statusErr.StatusCode < 500 || statusErr.StatusCode > 599) {
			return nil, err
		}
		if attempt == 2 {
			return nil, err
		}
		wait := time.Duration(1<<attempt) * time.Second
		if statusErr != nil {
			if statusErr.RetryAfter > 30*time.Second {
				return nil, err
			}
			wait = max(wait, statusErr.RetryAfter)
		}
		traceLogf(ctx, "│  │  image download retry attempt=%d wait=%s", attempt+2, wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, errors.New("image download failed")
}

func (c *UpstreamClient) downloadOnce(ctx context.Context, u string) ([]byte, error) {
	traceLogf(ctx, "│  ├─ download request %s", safeURLForLog(u))
	start := time.Now()
	parsedURL, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if !parsedURL.IsAbs() {
		base, _ := url.Parse(chatGPTBaseURL)
		parsedURL = base.ResolveReference(parsedURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, err
	}
	if parsedURL.Scheme == "https" && strings.EqualFold(parsedURL.Host, "chatgpt.com") {
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Referer", chatGPTBaseURL+"/")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header[http.HeaderOrderKey] = []string{"user-agent", "accept", "accept-language", "referer", "authorization"}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		traceLogf(ctx, "│  └─ download failed duration=%s url=%s", traceHTTPDuration(start), safeURLForLog(u))
		return nil, &imageDownloadTransportError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		traceLogf(ctx, "│  └─ download response status=%d duration=%s body=%s", resp.StatusCode, traceHTTPDuration(start), truncateText(string(b), 200))
		return nil, &imageDownloadStatusError{StatusCode: resp.StatusCode, Body: string(b), RetryAfter: retryAfterDuration(resp.Header.Get("Retry-After"), time.Now())}
	}
	data, err := io.ReadAll(resp.Body)
	traceLogf(ctx, "│  └─ download ok status=%d bytes=%d duration=%s", resp.StatusCode, len(data), traceHTTPDuration(start))
	if err == nil && len(data) == 0 {
		return nil, errors.New("download failed: empty response")
	}
	return data, err
}
func (c *UpstreamClient) DeleteConversation(ctx context.Context, cid string) {
	cid = strings.TrimSpace(cid)
	if cid == "" || c.token == "" {
		return
	}
	path := "/backend-api/conversation/" + cid
	resp, err := c.do(ctx, http.MethodPatch, path, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, map[string]any{"is_visible": false}, false)
	if err == nil && resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (c *UpstreamClient) ListModels(ctx context.Context) (map[string]any, error) {
	if err := c.bootstrap(ctx); err != nil {
		return nil, err
	}
	path := "/backend-api/models?history_and_training_disabled=false"
	route := "/backend-api/models"
	if c.token == "" {
		path = "/backend-anon/models?iim=false&is_gizmo=false"
		route = "/backend-anon/models"
	}
	resp, err := c.do(ctx, http.MethodGet, path, map[string]string{"Accept": "application/json", "X-OpenAI-Target-Path": route, "X-OpenAI-Target-Route": route}, nil, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	data := []map[string]any{}
	if models, ok := payload["models"].([]any); ok {
		for _, raw := range models {
			if m, ok := raw.(map[string]any); ok {
				slug := strings.TrimSpace(strAny(m["slug"], ""))
				if slug != "" && !seen[slug] {
					seen[slug] = true
					data = append(data, map[string]any{"id": slug, "object": "model", "created": intAny(m["created"], 0), "owned_by": strAny(m["owned_by"], "chatgpt"), "permission": []any{}, "root": slug, "parent": nil})
				}
			}
		}
	}
	for _, slug := range append(append([]string{}, webImageModels...), "codex-gpt-image-2") {
		if !seen[slug] {
			data = append(data, map[string]any{"id": slug, "object": "model", "created": time.Now().Unix(), "owned_by": "chatgpt2api-go", "permission": []any{}, "root": slug, "parent": nil})
		}
	}
	sort.Slice(data, func(i, j int) bool { return strAny(data[i]["id"], "") < strAny(data[j]["id"], "") })
	return map[string]any{"object": "list", "data": data}, nil
}

func (c *UpstreamClient) GetUserInfo(ctx context.Context) (Account, error) {
	if c.token == "" {
		return Account{}, errors.New("access_token is required")
	}
	me, meErr := c.getJSON(ctx, http.MethodGet, "/backend-api/me", map[string]string{"Accept": "application/json"}, nil)
	initPayload, initErr := c.getJSON(ctx, http.MethodPost, "/backend-api/conversation/init", map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, map[string]any{"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": -480})
	accountPayload, accountErr := c.getJSON(ctx, http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480", map[string]string{"Accept": "application/json", "X-OpenAI-Target-Path": "/backend-api/accounts/check/v4-2023-04-27", "X-OpenAI-Target-Route": "/backend-api/accounts/check/v4-2023-04-27"}, nil)
	checks := []struct {
		path string
		err  error
	}{
		{"GET /backend-api/me", meErr},
		{"POST /backend-api/conversation/init", initErr},
		{"GET /backend-api/accounts/check/v4-2023-04-27", accountErr},
	}
	for _, check := range checks {
		if isInvalidTokenErrorText(check.err) {
			return Account{}, fmt.Errorf("%s: %w", check.path, check.err)
		}
	}
	for _, check := range checks {
		if check.err != nil && check.path != "GET /backend-api/accounts/check/v4-2023-04-27" {
			return Account{}, fmt.Errorf("%s: %w", check.path, check.err)
		}
	}
	planType := ""
	trialState, trialPromoID, trialPromoLabel := "unknown", "", ""
	if accountErr == nil {
		var accountEntry map[string]any
		if accounts, ok := accountPayload["accounts"].(map[string]any); ok {
			accountEntry, _ = accounts[chatGPTAccountID(c.token)].(map[string]any)
			if accountEntry == nil {
				accountEntry, _ = accounts["default"].(map[string]any)
			}
		}
		if accountEntry != nil {
			if acct, ok := accountEntry["account"].(map[string]any); ok {
				if plan := strAny(acct["plan_type"], ""); plan != "" {
					planType = normalizeAccountType(plan)
				}
			}
			trialState, trialPromoID, trialPromoLabel = parseAccountTrial(accountEntry)
		}
	}
	quota, restoreAt, quotaUnknown, limits := 0, "", true, []map[string]any{}
	defaultModel := ""
	if arr, ok := initPayload["limits_progress"].([]any); ok {
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			limits = append(limits, m)
			if strAny(m["feature_name"], "") == "image_gen" {
				quota = intAny(m["remaining"], 0)
				restoreAt = strAny(m["reset_after"], strAny(m["resets_at"], ""))
				quotaUnknown = false
			}
		}
	}
	defaultModel = strAny(initPayload["default_model_slug"], "")
	status := "正常"
	if !quotaUnknown && quota <= 0 {
		status = "限流"
	}
	acc := Account{AccessToken: c.token, Type: planType, Status: status, SourceType: "web", Quota: quota, ImageQuotaUnknown: quotaUnknown, LimitsProgress: limits, TrialState: trialState, TrialPromoID: trialPromoID, TrialPromoLabel: trialPromoLabel, TrialCheckedAt: nowISO()}
	acc.QuotaCheckedAt = nowISO()
	if e := strAny(me["email"], ""); e != "" {
		acc.Email = &e
	}
	if uid := strAny(me["id"], ""); uid != "" {
		acc.UserID = &uid
	}
	if defaultModel != "" {
		acc.DefaultModelSlug = &defaultModel
	}
	if restoreAt != "" {
		acc.RestoreAt = &restoreAt
		acc.RateLimitResetAt = &restoreAt
	}
	if acc.InitialQuota < acc.Quota {
		acc.InitialQuota = acc.Quota
	}
	return acc, nil
}

func parseAccountTrial(entry map[string]any) (state, promoID, label string) {
	account, ok := entry["account"].(map[string]any)
	if !ok {
		return "unknown", "", ""
	}
	plan := strings.ToLower(strings.TrimSpace(strAny(account["plan_type"], "")))
	entitlement, _ := entry["entitlement"].(map[string]any)
	subscription := strings.ToLower(strings.TrimSpace(strAny(entitlement["subscription_plan"], "")))
	if (plan != "" && plan != "free") || (subscription != "" && subscription != "chatgptfreeplan") {
		return "subscribed", "", "已订阅"
	}
	campaigns, _ := entry["eligible_promo_campaigns"].(map[string]any)
	plus, ok := campaigns["plus"].(map[string]any)
	if !ok {
		return "not-eligible", "", "无优惠"
	}
	promoID = strings.TrimSpace(strAny(plus["id"], ""))
	if promoID == "" {
		return "unknown", "", ""
	}
	if promoID == "plus-1-month-free" {
		return "eligible", promoID, "一个月免费"
	}
	metadata, _ := plus["metadata"].(map[string]any)
	discount, _ := metadata["discount"].(map[string]any)
	duration, _ := metadata["duration"].(map[string]any)
	percentage := intAny(discount["percentage"], -1)
	periods := intAny(duration["num_periods"], 0)
	if percentage > 0 && percentage <= 100 && periods > 0 {
		return "discount", promoID, fmt.Sprintf("%d个月%g折", periods, 10-float64(percentage)/10)
	}
	return "discount", promoID, "优惠券"
}

func (c *UpstreamClient) getJSON(ctx context.Context, method, path string, headers map[string]string, body any) (map[string]any, error) {
	resp, err := c.do(ctx, method, path, headers, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

func parseSSE(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var lines []string
		flush := func() {
			if len(lines) == 0 {
				return
			}
			parts := []string{}
			for _, l := range lines {
				if strings.HasPrefix(l, "data:") {
					parts = append(parts, strings.TrimSpace(strings.TrimPrefix(l, "data:")))
				}
			}
			if len(parts) > 0 {
				ch <- strings.Join(parts, "\n")
			}
			lines = nil
		}
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				flush()
				continue
			}
			lines = append(lines, line)
		}
		flush()
	}()
	return ch
}
func normalizeChatGPTInternalMarkup(text string) string {
	if !strings.ContainsRune(text, '') {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		start := strings.IndexRune(text[i:], '')
		if start < 0 {
			b.WriteString(text[i:])
			break
		}
		start += i
		b.WriteString(text[i:start])
		end := strings.IndexRune(text[start:], '')
		if end < 0 {
			break
		}
		end += start
		b.WriteString(readableChatGPTInternalMark(text[start+len("") : end]))
		i = end + len("")
	}
	return b.String()
}

func readableChatGPTInternalMark(inner string) string {
	kind, payload, ok := strings.Cut(inner, "")
	if !ok {
		return ""
	}
	switch strings.TrimSpace(kind) {
	case "entity":
		return readableChatGPTEntity(payload)
	case "cite":
		return ""
	default:
		return ""
	}
}

func readableChatGPTEntity(payload string) string {
	payload = strings.TrimSpace(payload)
	payload = leadingJSONArray(payload)
	var parts []string
	if payload == "" || json.Unmarshal([]byte(payload), &parts) != nil || len(parts) == 0 {
		return ""
	}
	if len(parts) > 1 && isChatGPTEntityType(parts[0]) {
		parts = parts[1:]
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " - ")
}

func leadingJSONArray(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[") {
		return ""
	}
	inString := false
	escaped := false
	depth := 0
	for i, r := range text {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return text[:i+1]
			}
		}
	}
	return ""
}

func isChatGPTEntityType(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func assistantDelta(payload string, current *string) string {
	state := newUpstreamConversationState("", nil)
	state.Text = *current
	state.CleanText = *current
	meta, ok := state.Apply(payload)
	*current = state.Text
	if !ok {
		return ""
	}
	return meta.Delta
}

type upstreamConversationState struct {
	Text            string
	CleanText       string
	HistoryText     string
	HistoryMessages []string
	HistoryIndex    int
	ConversationID  string
	MessageID       string
	CurrentNode     string
	Blocked         bool
	ToolInvoked     any
	TurnUseCase     string
	FileIDs         []string
	SedimentIDs     []string
}

func newUpstreamConversationState(historyText string, historyMessages []string) *upstreamConversationState {
	return &upstreamConversationState{HistoryText: historyText, HistoryMessages: historyMessages}
}

func (s *upstreamConversationState) Apply(payload string) (UpstreamTextEvent, bool) {
	meta := upstreamTextMeta(payload)
	s.mergeMeta(meta)
	var ev map[string]any
	if json.Unmarshal([]byte(payload), &ev) != nil {
		meta = s.snapshotMeta()
		return meta, meta.ConversationID != "" || meta.MessageID != "" || meta.CurrentNode != ""
	}
	s.updateState(ev)
	if s.HistoryIndex < len(s.HistoryMessages) && eventAssistantText(ev, s.HistoryText) == s.HistoryMessages[s.HistoryIndex] {
		s.HistoryIndex++
		s.Text = ""
		s.CleanText = ""
		return s.snapshotMeta(), false
	}
	next := assistantTextWithHistory(ev, s.Text, s.HistoryText)
	if next == s.Text {
		meta = s.snapshotMeta()
		return meta, meta.ConversationID != "" || meta.MessageID != "" || meta.CurrentNode != "" || len(meta.FileIDs) > 0 || len(meta.SedimentIDs) > 0 || meta.Blocked || meta.ToolInvoked != nil || meta.TurnUseCase != ""
	}
	s.Text = next
	clean := normalizeChatGPTInternalMarkup(next)
	delta := ""
	if strings.HasPrefix(clean, s.CleanText) {
		delta = clean[len(s.CleanText):]
	} else {
		delta = clean
	}
	s.CleanText = clean
	meta = s.snapshotMeta()
	meta.Delta = delta
	return meta, delta != "" || meta.ConversationID != "" || meta.MessageID != "" || meta.CurrentNode != ""
}

func (s *upstreamConversationState) mergeMeta(meta UpstreamTextEvent) {
	if meta.ConversationID != "" {
		s.ConversationID = meta.ConversationID
	}
	if meta.MessageID != "" {
		s.MessageID = meta.MessageID
	}
	if meta.CurrentNode != "" {
		s.CurrentNode = meta.CurrentNode
	}
}

func (s *upstreamConversationState) snapshotMeta() UpstreamTextEvent {
	return UpstreamTextEvent{ConversationID: s.ConversationID, MessageID: s.MessageID, CurrentNode: s.CurrentNode, FileIDs: append([]string{}, s.FileIDs...), SedimentIDs: append([]string{}, s.SedimentIDs...), Blocked: s.Blocked, ToolInvoked: s.ToolInvoked, TurnUseCase: s.TurnUseCase}
}

func (s *upstreamConversationState) updateState(ev map[string]any) {
	meta := upstreamTextMetaFromMap(ev)
	s.mergeMeta(meta)
	payloadBytes, _ := json.Marshal(ev)
	payloadText := string(payloadBytes)
	isPatch := strAny(ev["o"], "") == "patch"
	isUser := isUserMessageEvent(ev)
	imageContext := isImageToolEvent(ev) || (s.ToolInvoked == true && !isUser) || (isPatch && !isUser && (strings.Contains(payloadText, "asset_pointer") || strings.Contains(payloadText, "file-service://") || strings.Contains(payloadText, "sediment://")))
	if imageContext {
		f, sed := imageIDsFromText(payloadText)
		s.FileIDs = unique(append(s.FileIDs, f...))
		s.SedimentIDs = unique(append(s.SedimentIDs, sed...))
	}
	if strAny(ev["type"], "") == "moderation" {
		if moderation, ok := ev["moderation_response"].(map[string]any); ok && boolAny(moderation["blocked"], false) {
			s.Blocked = true
		}
	}
	if strAny(ev["type"], "") == "server_ste_metadata" {
		if metadata, ok := ev["metadata"].(map[string]any); ok {
			if v, exists := metadata["tool_invoked"]; exists {
				if _, ok := v.(bool); ok {
					s.ToolInvoked = v
				}
			}
			if value := strings.TrimSpace(strAny(metadata["turn_use_case"], "")); value != "" {
				s.TurnUseCase = value
			}
		}
	}
}
func upstreamTextMeta(payload string) UpstreamTextEvent {
	meta := UpstreamTextEvent{}
	var ev map[string]any
	if json.Unmarshal([]byte(payload), &ev) != nil {
		if m := regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`).FindStringSubmatch(payload); len(m) > 1 {
			meta.ConversationID = m[1]
		}
		if m := regexp.MustCompile(`"message_id"\s*:\s*"([^"]+)"`).FindStringSubmatch(payload); len(m) > 1 {
			meta.MessageID = m[1]
		}
		if m := regexp.MustCompile(`"current_node"\s*:\s*"([^"]+)"`).FindStringSubmatch(payload); len(m) > 1 {
			meta.CurrentNode = m[1]
		}
		return meta
	}
	return upstreamTextMetaFromMap(ev)
}

func upstreamTextMetaFromMap(ev map[string]any) UpstreamTextEvent {
	meta := UpstreamTextEvent{}
	for _, cand := range []any{ev, ev["v"]} {
		m, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		if v := strings.TrimSpace(strAny(m["conversation_id"], "")); v != "" {
			meta.ConversationID = v
		}
		if v := strings.TrimSpace(strAny(m["current_node"], "")); v != "" {
			meta.CurrentNode = v
		}
		if msg, ok := m["message"].(map[string]any); ok {
			if v := strings.TrimSpace(strAny(msg["id"], "")); v != "" {
				meta.MessageID = v
			}
		}
	}
	return meta
}

func assistantTextFrom(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return assistantTextFromMap(m)
}

func assistantTextFromMap(m map[string]any) string {
	for _, cand := range []any{m, m["v"]} {
		cm, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := cm["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := msg["author"].(map[string]any)
		if strAny(author["role"], "") != "assistant" {
			continue
		}
		content, _ := msg["content"].(map[string]any)
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		b := strings.Builder{}
		for _, p := range parts {
			if s, ok := p.(string); ok {
				b.WriteString(s)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

func assistantTextWithHistory(event map[string]any, currentText, historyText string) string {
	if text := assistantTextFromMap(event); text != "" {
		return stripHistoryText(text, historyText)
	}
	if next, ok := applyTextPatchWithHistory(event, currentText, historyText); ok {
		return next
	}
	return currentText
}

func eventAssistantText(event map[string]any, historyText string) string {
	return stripHistoryText(normalizeChatGPTInternalMarkup(assistantTextFromMap(event)), historyText)
}

func stripHistoryText(text, historyText string) string {
	text = normalizeChatGPTInternalMarkup(text)
	historyText = normalizeChatGPTInternalMarkup(historyText)
	for historyText != "" && strings.HasPrefix(text, historyText) {
		text = text[len(historyText):]
	}
	return text
}

func applyTextPatchWithHistory(event map[string]any, current, historyText string) (string, bool) {
	if strAny(event["p"], "") == "/message/content/parts/0" {
		op := strAny(event["o"], "")
		value := strAny(event["v"], "")
		switch op {
		case "append":
			return current + value, true
		case "replace":
			return stripHistoryText(value, historyText), true
		}
	}
	if strAny(event["o"], "") == "patch" {
		ops, ok := event["v"].([]any)
		if !ok {
			return current, false
		}
		changed := false
		text := current
		for _, item := range ops {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if next, ok := applyTextPatchWithHistory(m, text, historyText); ok {
				text = next
				changed = true
			}
		}
		return text, changed
	}
	if ops, ok := event["v"].([]any); ok {
		changed := false
		text := current
		for _, item := range ops {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if next, ok := applyTextPatchWithHistory(m, text, historyText); ok {
				text = next
				changed = true
			}
		}
		return text, changed
	}
	if value, ok := event["v"].(string); ok && value != "" && current != "" && event["p"] == nil && event["o"] == nil {
		return current + value, true
	}
	return current, false
}

func assistantHistoryMessages(messages []map[string]any) []string {
	out := []string{}
	for _, message := range messages {
		if strings.TrimSpace(strAny(message["role"], "")) == "assistant" {
			if text := strings.TrimSpace(normalizeChatGPTInternalMarkup(messageTextAny(message["content"]))); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}
func isUserMessageEvent(ev map[string]any) bool {
	for _, cand := range []any{ev, ev["v"]} {
		m, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := m["message"].(map[string]any)
		if !ok {
			continue
		}
		if isUserMessage(msg) {
			return true
		}
	}
	return false
}

func isImageToolEvent(ev map[string]any) bool {
	for _, cand := range []any{ev, ev["v"]} {
		m, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := m["message"].(map[string]any)
		if !ok {
			continue
		}
		if isPythonImageToolMessage(msg) {
			return true
		}
	}
	return false
}

func imageIDsFromPayload(v any) ([]string, []string) {
	b, _ := json.Marshal(v)
	fileIDs, sedimentIDs := imageIDsFromText(string(b))
	fileIDs = append(fileIDs, bareFileAssetPointerIDs(v)...)
	return unique(fileIDs), unique(sedimentIDs)
}

func bareFileAssetPointerIDs(v any) []string {
	out := []string{}
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			asset := strings.TrimSpace(strAny(item["asset_pointer"], ""))
			if strings.HasPrefix(asset, "file_") && asset != "file_upload" {
				out = append(out, asset)
			}
			for _, child := range item {
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(v)
	return unique(out)
}

func imageIDsFromText(text string) ([]string, []string) {
	fileIDs := []string{}
	sedimentIDs := []string{}
	for _, m := range regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			fileIDs = append(fileIDs, m[1])
		}
	}
	for _, m := range regexp.MustCompile(`\bfile_00000000[a-f0-9]{24}\b`).FindAllStringSubmatch(text, -1) {
		if len(m) == 0 {
			continue
		}
		fileIDs = append(fileIDs, m[0])
	}
	for _, m := range regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			sedimentIDs = append(sedimentIDs, m[1])
		}
	}
	return unique(fileIDs), unique(sedimentIDs)
}

func updateImageState(payload string, cid *string, message *string) {
	if *cid == "" {
		if m := regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`).FindStringSubmatch(payload); len(m) > 1 {
			*cid = m[1]
		}
	}
	var ev map[string]any
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if t := assistantTextFrom(ev); t != "" {
		*message = t
	}
	if t, ok := applyTextPatch(ev, *message); ok {
		*message = t
	}
}

func applyTextPatch(event map[string]any, current string) (string, bool) {
	if strAny(event["p"], "") == "/message/content/parts/0" {
		op := strAny(event["o"], "")
		value := strAny(event["v"], "")
		switch op {
		case "append":
			return current + value, true
		case "replace":
			return value, true
		}
	}
	if strAny(event["o"], "") == "patch" {
		ops, ok := event["v"].([]any)
		if !ok {
			return current, false
		}
		changed := false
		text := current
		for _, item := range ops {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if next, ok := applyTextPatch(m, text); ok {
				text = next
				changed = true
			}
		}
		return text, changed
	}
	if value, ok := event["v"].(string); ok && value != "" && current != "" && event["p"] == nil && event["o"] == nil {
		return current + value, true
	}
	return current, false
}

func isImageQuotaMessage(s string) bool {
	text := strings.ToLower(s)
	return strings.Contains(text, "free plan limit") ||
		strings.Contains(text, "limit for image generation") ||
		strings.Contains(text, "image generations requests") ||
		strings.Contains(text, "when the limit resets") ||
		strings.Contains(text, "usage limit") ||
		strings.Contains(text, "图像生成请求上限") ||
		strings.Contains(text, "图像生成次数上限") ||
		strings.Contains(text, "图片生成次数上限")
}

func imagePollQuotaMessage(conv map[string]any) string {
	mapping, _ := conv["mapping"].(map[string]any)
	nodeID := strAny(conv["current_node"], "")
	visited := map[string]bool{}
	for nodeID != "" && !visited[nodeID] {
		visited[nodeID] = true
		node, _ := mapping[nodeID].(map[string]any)
		message, _ := node["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		role := strAny(author["role"], "")
		if role == "user" {
			break
		}
		if role == "assistant" {
			content, _ := message["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			var text strings.Builder
			for _, part := range parts {
				text.WriteString(strAny(part, ""))
			}
			if isImageQuotaMessage(text.String()) {
				return text.String()
			}
		}
		nodeID = strAny(node["parent"], "")
	}
	return ""
}

func cleanImageMessage(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	for _, needle := range []string{"you've hit", "you have hit", "free plan limit", "usage limit"} {
		if i := strings.Index(lower, needle); i >= 0 {
			return strings.TrimSpace(s[i:])
		}
	}
	return s
}
func extractToolIDs(conv map[string]any) ([]string, []string) {
	fileIDs, sedimentIDs := extractPythonImageToolIDs(conv)
	fallbackFileIDs, fallbackSedimentIDs := extractFallbackImageIDs(conv)
	fileIDs = append(fileIDs, fallbackFileIDs...)
	sedimentIDs = append(sedimentIDs, fallbackSedimentIDs...)
	return unique(fileIDs), unique(sedimentIDs)
}

func extractPythonImageToolIDs(conv map[string]any) ([]string, []string) {
	fileIDs := []string{}
	sedimentIDs := []string{}
	filePat := regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	sedPat := regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
	if mapping, ok := conv["mapping"].(map[string]any); ok {
		for _, rawNode := range mapping {
			node, ok := rawNode.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := node["message"].(map[string]any)
			if !ok || !isPythonImageToolMessage(msg) {
				continue
			}
			content, _ := msg["content"].(map[string]any)
			if parts, ok := content["parts"].([]any); ok {
				for _, part := range parts {
					text := ""
					if m, ok := part.(map[string]any); ok {
						text = strAny(m["asset_pointer"], "")
					} else if s, ok := part.(string); ok {
						text = s
					}
					for _, m := range filePat.FindAllStringSubmatch(text, -1) {
						if len(m) > 1 {
							fileIDs = append(fileIDs, m[1])
						}
					}
					for _, m := range sedPat.FindAllStringSubmatch(text, -1) {
						if len(m) > 1 {
							sedimentIDs = append(sedimentIDs, m[1])
						}
					}
				}
			}
		}
	}
	return unique(fileIDs), unique(sedimentIDs)
}

func extractFallbackImageIDs(conv map[string]any) ([]string, []string) {
	fileIDs := []string{}
	sedimentIDs := []string{}
	if mapping, ok := conv["mapping"].(map[string]any); ok {
		for _, rawNode := range mapping {
			node, ok := rawNode.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := node["message"].(map[string]any)
			if !ok || isUserMessage(msg) {
				continue
			}
			if !isFallbackImageResultMessage(msg) && !looksLikeImageResultMessage(msg) {
				continue
			}
			f, s := imageIDsFromPayload(msg)
			fileIDs = append(fileIDs, f...)
			sedimentIDs = append(sedimentIDs, s...)
		}
	}
	return unique(fileIDs), unique(sedimentIDs)
}

func isUserMessage(msg map[string]any) bool {
	author, _ := msg["author"].(map[string]any)
	return strAny(author["role"], "") == "user"
}

func looksLikeImageResultMessage(msg map[string]any) bool {
	author, _ := msg["author"].(map[string]any)
	metadata, _ := msg["metadata"].(map[string]any)
	content, _ := msg["content"].(map[string]any)
	role := strings.TrimSpace(strings.ToLower(strAny(author["role"], "")))
	if role != "tool" && role != "assistant" {
		return false
	}
	isImageGen := strAny(metadata["async_task_type"], "") == "image_gen"
	hasAsset := hasImageAssetPointer(content) || hasImageAssetPointer(metadata)
	if role == "assistant" && !isImageGen && !hasAsset {
		return false
	}
	if isImageGen || hasAsset {
		return true
	}
	f, s := imageIDsFromPayload(map[string]any{"content": content, "metadata": metadata})
	return len(f) > 0 || len(s) > 0
}

func hasImageAssetPointer(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if strAny(v["content_type"], "") == "image_asset_pointer" {
			return true
		}
		asset := strAny(v["asset_pointer"], "")
		if strings.HasPrefix(asset, "file-service://") || strings.HasPrefix(asset, "sediment://") {
			return true
		}
		for _, item := range v {
			if hasImageAssetPointer(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if hasImageAssetPointer(item) {
				return true
			}
		}
	}
	return false
}

func isPythonImageToolMessage(msg map[string]any) bool {
	author, _ := msg["author"].(map[string]any)
	metadata, _ := msg["metadata"].(map[string]any)
	content, _ := msg["content"].(map[string]any)
	return strAny(author["role"], "") == "tool" &&
		strAny(metadata["async_task_type"], "") == "image_gen" &&
		strAny(content["content_type"], "") == "multimodal_text"
}

func isFallbackImageResultMessage(msg map[string]any) bool {
	author, _ := msg["author"].(map[string]any)
	metadata, _ := msg["metadata"].(map[string]any)
	content, _ := msg["content"].(map[string]any)
	role := strAny(author["role"], "")
	asyncTaskType := strAny(metadata["async_task_type"], "")
	contentType := strAny(content["content_type"], "")
	if role == "user" {
		return false
	}
	if role == "tool" && asyncTaskType == "image_gen" {
		return true
	}
	if role == "assistant" && (asyncTaskType == "image_gen" || contentType == "multimodal_text") {
		f, s := imageIDsFromPayload(msg)
		return len(f) > 0 || len(s) > 0
	}
	return false
}
func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimPrefix(v, "sediment://")
		v = strings.TrimPrefix(v, "file-service://")
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
func buildImagePrompt(prompt, size string) string {
	p := strings.TrimSpace(prompt)
	if strings.TrimSpace(size) == "" {
		return p
	}
	switch strings.TrimSpace(size) {
	case "1:1":
		return p + "\n\n输出为 1:1 正方形构图，主体居中，适合正方形画幅。"
	case "16:9":
		return p + "\n\n输出为 16:9 横屏构图，适合宽画幅展示。"
	case "9:16":
		return p + "\n\n输出为 9:16 竖屏构图，适合竖版画幅展示。"
	case "4:3":
		return p + "\n\n输出为 4:3 比例，兼顾宽度与高度，适合展示画面细节。"
	case "3:4":
		return p + "\n\n输出为 3:4 比例，纵向构图，适合人物肖像或竖向场景。"
	default:
		return p + "\n\n输出图片，宽高比为 " + strings.TrimSpace(size) + "。"
	}
}

func normalizeImageResolution(value string) string {
	n := strings.ToLower(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(value)))
	switch n {
	case "auto", "default", "":
		return ""
	case "1", "1024", "1024px", "1024x1024", "1k":
		return "1k"
	case "2", "2048", "2048px", "2048x2048", "2k":
		return "2k"
	case "4", "3840", "3840px", "3840x2160", "2160x3840", "4096", "4096px", "4096x4096", "4k":
		return "4k"
	default:
		return ""
	}
}

func buildImagePromptWithOptions(prompt, size, resolution string) string {
	final := strings.TrimSpace(buildImagePrompt(prompt, size))
	switch normalizeImageResolution(resolution) {
	case "1k":
		return final + "\n\n目标输出分辨率为 1K 级别，优先保证构图稳定和主体清晰。"
	case "2k":
		return final + "\n\n目标输出分辨率为 2K 级别，尽可能输出长边约 2048px 的高清图片，保留细节纹理。"
	case "4k":
		return final + "\n\n目标输出分辨率为 4K 级别，尽可能输出接近 3840px 长边的超清图片，保留丰富细节和干净边缘。"
	default:
		return final
	}
}

func normalizeAccountType(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "free"
	}
	key := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(raw))
	switch key {
	case "free":
		return "free"
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "prolite":
		return "ProLite"
	case "team", "business":
		return "Team"
	case "enterprise":
		return "Enterprise"
	default:
		return raw
	}
}

type textModelOptions struct {
	Model          string
	ReasoningLevel string
}

func normalizeTextModelOptions(model string) textModelOptions {
	slug := strings.TrimSpace(strings.ToLower(model))
	switch slug {
	case "", "auto":
		return textModelOptions{Model: "auto"}
	case "gpt-5-1", "gpt-5-mini":
		return textModelOptions{Model: "gpt-5", ReasoningLevel: "low"}
	case "gpt-5-2":
		return textModelOptions{Model: "gpt-5", ReasoningLevel: "medium"}
	case "gpt-5-3", "gpt-5-3-mini":
		return textModelOptions{Model: "gpt-5", ReasoningLevel: "high"}
	default:
		return textModelOptions{Model: model}
	}
}

func imageModelSlug(model string) string {
	if isWebImageModel(model) {
		return "auto"
	}
	if isCodexImageModel(model) {
		return "codex-gpt-image-2"
	}
	return "auto"
}

func (c *UpstreamClient) imageModelSlug(model string) string {
	slug := imageModelSlug(model)
	if slug == "auto" && c.defaultModelSlug != "" {
		return c.defaultModelSlug
	}
	return slug
}
func clientContext() map[string]any {
	return map[string]any{"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900, "page_width": 1400, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"}
}
func (c *UpstreamClient) toConversationMessages(ctx context.Context, messages []map[string]any) ([]map[string]any, error) {
	if len(messages) == 0 {
		messages = []map[string]any{{"role": "user", "content": "Hello"}}
	}
	out := []map[string]any{}
	for _, m := range messages {
		role, rawContent := upstreamConversationRoleAndContent(m)
		content, metadata, err := c.conversationContent(ctx, rawContent)
		if err != nil {
			return nil, err
		}
		msg := map[string]any{"id": uuid4(), "author": map[string]any{"role": role}, "content": content}
		if len(metadata) > 0 {
			msg["metadata"] = metadata
		}
		out = append(out, msg)
	}
	return out, nil
}

func upstreamConversationRoleAndContent(m map[string]any) (string, any) {
	role := strings.ToLower(strings.TrimSpace(strAny(m["role"], "user")))
	switch role {
	case "assistant", "user":
		return role, m["content"]
	case "system", "developer":
		return "user", "System instructions:\n" + messageTextAny(m["content"])
	case "tool", "function":
		name := firstNonEmpty(strAny(m["name"], ""), strAny(m["tool_call_id"], ""), strAny(m["tool_use_id"], ""), "tool")
		return "user", "Tool result from " + name + ":\n" + messageTextAny(m["content"])
	default:
		return "user", messageTextAny(m["content"])
	}
}

func (c *UpstreamClient) conversationContent(ctx context.Context, raw any) (map[string]any, map[string]any, error) {
	text := messageTextAny(raw)
	images := extractImagesFromContent(raw)
	if len(images) == 0 {
		return map[string]any{"content_type": "text", "parts": []string{text}}, nil, nil
	}
	parts := []any{}
	attachments := []map[string]any{}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	}
	for i, imageBytes := range images {
		uploaded, err := c.uploadImage(ctx, imageBytes, fmt.Sprintf("chat_image_%d.png", i+1), 120*time.Second)
		if err != nil {
			return nil, nil, err
		}
		fileID := strAny(uploaded["file_id"], "")
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + fileID, "width": uploaded["width"], "height": uploaded["height"], "size_bytes": uploaded["file_size"]})
		attachments = append(attachments, map[string]any{"id": fileID, "mimeType": uploaded["mime_type"], "name": uploaded["file_name"], "size": uploaded["file_size"], "width": uploaded["width"], "height": uploaded["height"]})
	}
	metadata := map[string]any{"attachments": attachments}
	return map[string]any{"content_type": "multimodal_text", "parts": parts}, metadata, nil
}

func parsePowResources(html string) ([]string, string) {
	re := regexp.MustCompile(`<script[^>]+src=["']([^"']+)["']`)
	matches := re.FindAllStringSubmatch(html, -1)
	src := []string{}
	build := ""
	for _, m := range matches {
		src = append(src, m[1])
		if build == "" {
			if mm := regexp.MustCompile(`c/[^/]*/_`).FindStringSubmatch(m[1]); len(mm) > 0 {
				build = mm[0]
			}
		}
	}
	if build == "" {
		if m := regexp.MustCompile(`<html[^>]*data-build=["']([^"']*)`).FindStringSubmatch(html); len(m) > 1 {
			build = m[1]
		}
	}
	return src, build
}

func localTimezoneName() string {
	for _, value := range []string{os.Getenv("CHATGPT2API_UPSTREAM_TIMEZONE"), os.Getenv("TZ")} {
		if name := strings.TrimSpace(value); name != "" {
			return name
		}
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(link, "/zoneinfo/"); ok {
			return name
		}
	}
	return "Asia/Shanghai"
}

func (c *UpstreamClient) timezoneInfo() (string, int, *time.Location) {
	name := c.timezone
	if name == "" {
		name = localTimezoneName()
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		name = "Asia/Shanghai"
		location, _ = time.LoadLocation(name)
	}
	_, offsetSeconds := time.Now().In(location).Zone()
	return name, -offsetSeconds / 60, location
}

func (c *UpstreamClient) powDate() string {
	if c.browserDate != "" {
		return c.browserDate
	}
	name, _, location := c.timezoneInfo()
	date := time.Now().In(location)
	label, _ := date.Zone()
	switch name {
	case "Asia/Shanghai":
		label = "China Standard Time"
	case "America/Los_Angeles":
		if label == "PDT" {
			label = "Pacific Daylight Time"
		} else {
			label = "Pacific Standard Time"
		}
	}
	return date.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (" + label + ")"
}

func buildPowConfig(ua string, scripts []string, dataBuild, date string, identity sentinelIdentity) []any {
	nav := []string{"webdriver−false", "vendor−Google Inc.", "hardwareConcurrency−32", "language−zh-CN", "cookieEnabled−true"}
	win := []string{"window", "document", "location", "navigator", "crypto", "fetch", "screen"}
	script := defaultPowScript
	if len(scripts) > 0 {
		script = scripts[rand.Intn(len(scripts))]
	}
	now := time.Now()
	cfg := []any{[]int{3000, 4000, 5000}[rand.Intn(3)], date, int64(4294705152), 1, ua, script, dataBuild, "en-US", "en-US,es-US,en,es", rand.Float64(), nav[rand.Intn(len(nav))], "location", win[rand.Intn(len(win))], float64(now.UnixNano()) / 1e6, uuid4(), "", []int{8, 16, 24, 32}[rand.Intn(4)], float64(now.UnixNano())/1e6 - 1000, 0, 0, 0, 0, 0, 0, 0}
	if identity.ScreenSize > 0 {
		cfg[0] = identity.ScreenSize
	}
	if identity.HeapLimit > 0 {
		cfg[2] = identity.HeapLimit
	}
	if identity.Language != "" {
		cfg[7] = identity.Language
	}
	if identity.Languages != "" {
		cfg[8] = identity.Languages
	}
	if identity.Vendor != "" {
		cfg[10] = "vendor−" + identity.Vendor
	}
	if identity.HardwareConcurrency > 0 {
		cfg[16] = identity.HardwareConcurrency
	}
	return cfg
}
func powGenerate(seed, diff string, cfg []any, limit int) (string, bool) {
	target := strings.ToLower(strings.TrimSpace(diff))
	if target == "" {
		return "", false
	}
	started := time.Now()
	for i := 0; i < limit; i++ {
		cfg[3] = i
		cfg[9] = time.Since(started).Milliseconds()
		enc := base64.StdEncoding.EncodeToString([]byte(jsonCompact(cfg)))
		digest := fmt.Sprintf("%08x", fnv1a32(seed+enc))
		if len(target) <= len(digest) && digest[:len(target)] <= target {
			return enc, true
		}
	}
	return "", false
}
func fnv1a32(s string) uint32 {
	h := uint32(2166136261)
	for _, ch := range s {
		h ^= uint32(ch)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return h
}
func buildLegacyRequirementsToken(ua string, scripts []string, dataBuild, date string, identity sentinelIdentity) string {
	return "gAAAAAC" + base64.StdEncoding.EncodeToString([]byte(jsonCompact(buildPowConfig(ua, scripts, dataBuild, date, identity))))
}
func buildProofToken(seed, diff, ua string, scripts []string, dataBuild, date string, identity sentinelIdentity) (string, error) {
	ans, ok := powGenerate(seed, diff, buildPowConfig(ua, scripts, dataBuild, date, identity), 500000)
	if !ok {
		return "", fmt.Errorf("failed to solve proof token: difficulty=%s", diff)
	}
	return "gAAAAAB" + ans + "~S", nil
}
func jsonCompact(v any) string { b, _ := json.Marshal(v); return string(b) }
func hexDecode(s string) ([]byte, error) {
	if len(s)%2 == 1 {
		s = "0" + s
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var x byte
		for _, c := range []byte{s[2*i], s[2*i+1]} {
			x <<= 4
			switch {
			case c >= '0' && c <= '9':
				x += c - '0'
			case c >= 'a' && c <= 'f':
				x += c - 'a' + 10
			case c >= 'A' && c <= 'F':
				x += c - 'A' + 10
			default:
				return nil, fmt.Errorf("bad hex")
			}
		}
		out[i] = x
	}
	return out, nil
}

func (s *Server) upstreamToken() (string, error) {
	return s.pickTextToken()
}

func (s *Server) upstreamClient() (*UpstreamClient, error) {
	account, err := s.pickTextAccountExcluding(nil, "")
	if err != nil {
		return nil, err
	}
	return s.newUpstreamClientForAccount(account)
}
func (s *Server) upstreamClientForImage(model, resolution string) (*UpstreamClient, error) {
	return s.upstreamClientForImageExcluding(model, resolution, nil)
}

func (s *Server) upstreamClientForImageExcluding(model, resolution string, excluded map[string]bool) (*UpstreamClient, error) {
	account, err := s.pickAccountExcluding(model, resolution, excluded)
	if err != nil {
		return nil, err
	}
	client, _, err := s.upstreamClientForImageAccount(model, resolution, account)
	return client, err
}

func (s *Server) upstreamClientForImageAccount(model, resolution string, account Account) (*UpstreamClient, Account, error) {
	if isCodexImageRequest(model, resolution) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if refreshed, err := s.refreshOAuthAccount(ctx, account.AccessToken); err == nil && refreshed.AccessToken != "" {
			account = refreshed
		}
	}
	client, err := s.newUpstreamClientForAccount(account)
	return client, account, err
}

func init() { rand.Seed(time.Now().UnixNano()) }
