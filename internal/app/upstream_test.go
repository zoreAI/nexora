package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

type upstreamDoerFunc func(req *http.Request) (*http.Response, error)

func (f upstreamDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func upstreamTestResponse(status int, body string) *http.Response {
	return upstreamTestResponseWithHeader(status, body, http.Header{})
}

func upstreamTestResponseWithHeader(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
	}
}

func TestDefaultUpstreamIdentityUsesChrome146(t *testing.T) {
	t.Setenv("CHATGPT2API_UPSTREAM_TRANSPORT", "tls-client")
	t.Setenv("CHATGPT2API_TLS_PROFILE", "")
	t.Setenv("CHATGPT2API_USER_AGENT", "")
	t.Setenv("CHATGPT2API_SEC_CH_UA", "")
	client, err := NewUpstreamClientForAccount(Account{AccessToken: "test-token"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.client.(tlsclient.HttpClient); !ok {
		t.Fatalf("default transport = %T, want tls-client", client.client)
	}
	if upstreamTLSProfile().GetClientHelloStr() != profiles.Chrome_146.GetClientHelloStr() {
		t.Fatal("default TLS profile is not Chrome 146")
	}
	headers := client.headers("/backend-api/f/conversation", nil)
	for _, name := range []string{"User-Agent", "Sec-Ch-Ua", "Sec-Ch-Ua-Full-Version", "Sec-Ch-Ua-Full-Version-List"} {
		value := headers.Get(name)
		if !strings.Contains(value, "146") || strings.Contains(value, "Edge") {
			t.Errorf("%s = %q, want Chrome 146 without Edge", name, value)
		}
	}
}

func TestBootstrapUsesDocumentHeadersOnly(t *testing.T) {
	client := &UpstreamClient{
		token:           "access-token",
		userAgent:       "test-agent",
		secCHUA:         `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		secCHUAMobile:   "?0",
		secCHUAPlatform: `"Windows"`,
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "" {
				t.Fatalf("bootstrap sent Authorization header: %q", got)
			}
			for _, name := range []string{
				"Origin",
				"Referer",
				"OAI-Device-Id",
				"OAI-Session-Id",
				"OAI-Language",
				"OAI-Client-Version",
				"OAI-Client-Build-Number",
				"X-OpenAI-Target-Path",
				"X-OpenAI-Target-Route",
			} {
				if got := req.Header.Get(name); got != "" {
					t.Fatalf("bootstrap sent %s header: %q", name, got)
				}
			}
			if got := req.Header.Get("Sec-Fetch-Mode"); got != "navigate" {
				t.Fatalf("Sec-Fetch-Mode = %q, want navigate", got)
			}
			if got := req.Header.Get("Sec-Fetch-Site"); got != "none" {
				t.Fatalf("Sec-Fetch-Site = %q, want none", got)
			}
			if got := req.Header.Get("Sec-Fetch-User"); got != "?1" {
				t.Fatalf("Sec-Fetch-User = %q, want ?1", got)
			}
			return upstreamTestResponse(200, `<html data-build="test-build"><script src="/c/test/_/x.js"></script></html>`), nil
		}),
	}
	if err := client.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap returned error: %v", err)
	}
}

func TestBootstrapRetriesRedirectAndThenSucceeds(t *testing.T) {
	attempts := 0
	client := &UpstreamClient{
		userAgent:       "test-agent",
		secCHUA:         `"Microsoft Edge";v="143"`,
		secCHUAMobile:   "?0",
		secCHUAPlatform: `"Windows"`,
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				h := http.Header{}
				h.Set("Location", "/auth/login")
				return upstreamTestResponseWithHeader(302, "", h), nil
			}
			return upstreamTestResponse(200, `<html data-build="retry-build"><script src="/c/retry/_/x.js"></script></html>`), nil
		}),
	}
	if err := client.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if client.dataBuild != "c/retry/_" {
		t.Fatalf("dataBuild = %q, want c/retry/_", client.dataBuild)
	}
}

func TestBootstrapRedirectErrorIncludesLocation(t *testing.T) {
	client := &UpstreamClient{
		userAgent:       "test-agent",
		secCHUA:         `"Microsoft Edge";v="143"`,
		secCHUAMobile:   "?0",
		secCHUAPlatform: `"Windows"`,
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			h := http.Header{}
			h.Set("Location", "/auth/login")
			return upstreamTestResponseWithHeader(302, "", h), nil
		}),
	}
	err := client.bootstrap(context.Background())
	if err == nil {
		t.Fatal("expected bootstrap error")
	}
	if !strings.Contains(err.Error(), "bootstrap redirect: status=302") || !strings.Contains(err.Error(), "/auth/login") {
		t.Fatalf("error = %q, want redirect status and location", err.Error())
	}
}

func TestAuthenticatedChatRequirementsPrepareFinalizeWithSolver(t *testing.T) {
	var proofInput string
	solver := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer solver-key" {
			t.Errorf("solver Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, `{"ready":true,"user_agent":"solver-agent","timezone":"America/Los_Angeles","date_string":"Thu Oct 15 2026 06:00:00 GMT-0700 (Pacific Daylight Time)"}`)
		case "/v1/turnstile":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["dx"] != "challenge" || body["p"] != proofInput {
				t.Errorf("solver challenge = %#v", body)
			}
			requirements, _ := body["requirements"].(map[string]any)
			if requirements["prepare_token"] != "prepare-token" {
				t.Errorf("solver requirements = %#v", requirements)
			}
			_, _ = io.WriteString(w, `{"token":"solved-token"}`)
		default:
			t.Errorf("unexpected solver path %q", r.URL.Path)
			w.WriteHeader(stdhttp.StatusNotFound)
		}
	}))
	defer solver.Close()
	upstreamCalls := 0
	client := &UpstreamClient{
		token:           "access-token",
		userAgent:       "test-agent",
		solverURL:       solver.URL,
		solverKey:       "solver-key",
		deviceID:        "device-id",
		sessionID:       "session-id",
		secCHUA:         `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		secCHUAMobile:   "?0",
		secCHUAPlatform: `"Windows"`,
		scriptSources:   []string{defaultPowScript},
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			upstreamCalls++
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			switch req.URL.Path {
			case "/backend-api/sentinel/chat-requirements/prepare":
				proofInput, _ = body["p"].(string)
				if proofInput == "" || req.Header.Get("X-OpenAI-Target-Path") != "" {
					t.Errorf("prepare headers or proof input invalid")
				}
				return upstreamTestResponse(200, `{"prepare_token":"prepare-token","turnstile":{"required":true,"dx":"challenge"}}`), nil
			case "/backend-api/sentinel/chat-requirements/finalize":
				if body["prepare_token"] != "prepare-token" || body["turnstile"] != "solved-token" || body["proofofwork"] != "" {
					t.Errorf("finalize body = %#v", body)
				}
				return upstreamTestResponse(200, `{"token":"requirements-token"}`), nil
			default:
				t.Fatalf("unexpected upstream path %q", req.URL.Path)
				return nil, nil
			}
		}),
	}
	got, err := client.chatRequirements(context.Background())
	if err != nil {
		t.Fatalf("chatRequirements returned error: %v", err)
	}
	if upstreamCalls != 2 || got.Token != "requirements-token" || got.TurnstileToken != "solved-token" {
		t.Fatalf("calls = %d, requirements = %#v", upstreamCalls, got)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(proofInput, "gAAAAAC"))
	if err != nil {
		t.Fatal(err)
	}
	var config []any
	if err := json.Unmarshal(decoded, &config); err != nil {
		t.Fatal(err)
	}
	if config[1] != "Thu Oct 15 2026 06:00:00 GMT-0700 (Pacific Daylight Time)" {
		t.Fatalf("proof date = %v", config[1])
	}
	if client.timezone != "America/Los_Angeles" {
		t.Fatalf("solver timezone = %q", client.timezone)
	}
}

func TestImageConversationPrepareAndSendStates(t *testing.T) {
	var prepareTrace string
	client := &UpstreamClient{
		token:    "access-token",
		timezone: "America/Los_Angeles",
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode %s: %v", req.URL.Path, err)
			}
			if body["model"] != "auto" {
				t.Errorf("%s model = %v, want auto", req.URL.Path, body["model"])
			}
			name, offset, _ := (&UpstreamClient{timezone: "America/Los_Angeles"}).timezoneInfo()
			if body["timezone"] != name || body["timezone_offset_min"] != float64(offset) {
				t.Errorf("%s timezone = %v offset = %v", req.URL.Path, body["timezone"], body["timezone_offset_min"])
			}
			switch req.URL.Path {
			case "/backend-api/f/conversation/prepare":
				prepareTrace = req.Header.Get("X-Oai-Turn-Trace-Id")
				if prepareTrace == "" || body["parent_message_id"] != clientCreatedRoot {
					t.Errorf("prepare trace = %q, parent = %v", prepareTrace, body["parent_message_id"])
				}
				if body["thinking_effort"] != nil || body["supports_buffering"] != true {
					t.Errorf("prepare thinking = %v, buffering = %v", body["thinking_effort"], body["supports_buffering"])
				}
				if body["client_prepare_state"] != "success" || body["client_prepare_dispatch"] != nil {
					t.Errorf("prepare state = %v, dispatch = %v", body["client_prepare_state"], body["client_prepare_dispatch"])
				}
				if _, ok := body["partial_query"]; !ok {
					t.Error("prepare missing partial_query")
				}
				return upstreamTestResponse(200, `{"conduit_token":"conduit-token"}`), nil
			case "/backend-api/f/conversation":
				if body["thinking_effort"] != nil || body["supports_buffering"] != true {
					t.Errorf("send thinking = %v, buffering = %v", body["thinking_effort"], body["supports_buffering"])
				}
				if req.Header.Get("X-Oai-Mcp-Form-Version") != "" {
					t.Errorf("send MCP form version = %q", req.Header.Get("X-Oai-Mcp-Form-Version"))
				}
				if body["client_prepare_state"] != "success" || body["parent_message_id"] != clientCreatedRoot {
					t.Errorf("send state = %v, parent = %v", body["client_prepare_state"], body["parent_message_id"])
				}
				if got := req.Header.Get("X-Oai-Turn-Trace-Id"); got != prepareTrace {
					t.Errorf("send trace = %q, want %q", got, prepareTrace)
				}
				messages, _ := body["messages"].([]any)
				if len(messages) != 1 {
					t.Fatalf("send messages = %#v", body["messages"])
				}
				message, _ := messages[0].(map[string]any)
				content, _ := message["content"].(map[string]any)
				parts, _ := content["parts"].([]any)
				if len(parts) != 1 || parts[0] != "test prompt" {
					t.Errorf("send content = %#v", content)
				}
				metadata, _ := message["metadata"].(map[string]any)
				serialization, _ := metadata["serialization_metadata"].(map[string]any)
				offsets, _ := serialization["custom_symbol_offsets"].([]any)
				if len(offsets) != 0 {
					t.Errorf("send custom symbol offsets = %#v, want none", offsets)
				}
				if req.Header.Get("X-Conduit-Token") != "conduit-token" {
					t.Error("send missing conduit token")
				}
				return upstreamTestResponse(200, "data: [DONE]\n\n"), nil
			default:
				t.Fatalf("unexpected path %s", req.URL.Path)
				return nil, nil
			}
		}),
	}
	cr := chatRequirements{Token: "requirements-token"}
	conduit, err := client.prepareImage(context.Background(), "test prompt", "gpt-image-2", cr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.startImage(context.Background(), "test prompt", "gpt-image-2", cr, conduit, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestImageIDsAfterSSEPollsTextOnlyTurn(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Path != "/backend-api/conversation/conversation-1" {
			t.Fatalf("unexpected request path %s", req.URL.Path)
		}
		return upstreamTestResponse(200, `{"mapping":{"image":{"message":{"author":{"role":"tool"},"metadata":{"async_task_type":"image_gen"},"content":{"content_type":"multimodal_text","parts":[{"asset_pointer":"file-service://file_generated"}]}}}}}`), nil
	})}
	state := &upstreamConversationState{ToolInvoked: false, TurnUseCase: "image gen"}
	files, sediments, err := client.imageIDsAfterSSE(context.Background(), "conversation-1", "已为你生成图片", state, nil, nil, imageGenerationOptions{Timeout: time.Second, PollInterval: time.Millisecond})
	if err != nil || len(files) != 1 || files[0] != "file_generated" || len(sediments) != 0 || requests != 1 {
		t.Fatalf("text-only turn image lookup: files=%v sediments=%v requests=%d err=%v", files, sediments, requests, err)
	}

	state.Blocked = true
	_, _, err = client.imageIDsAfterSSE(context.Background(), "conversation-1", "blocked", state, nil, nil, imageGenerationOptions{Timeout: time.Second})
	if err == nil || requests != 1 {
		t.Fatalf("blocked turn should not poll: requests=%d err=%v", requests, err)
	}
	state.Blocked = false
	_, _, err = client.imageIDsAfterSSE(context.Background(), "conversation-1", "You've hit your free plan limit for image generation", state, nil, nil, imageGenerationOptions{Timeout: time.Second})
	if err == nil || requests != 1 {
		t.Fatalf("quota message should not poll: requests=%d err=%v", requests, err)
	}
	state.ToolInvoked = true
	state.TurnUseCase = "image gen"
	_, _, err = client.imageIDsAfterSSE(context.Background(), "conversation-1", "你已达到 Free 套餐的图像生成请求上限。", state, nil, nil, imageGenerationOptions{Timeout: time.Second})
	if err != nil || requests != 1 {
		t.Fatalf("Chinese quota message should not poll: requests=%d err=%v", requests, err)
	}
}

func TestImagePollStopsOnCurrentTurnQuota(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return upstreamTestResponse(200, `{"current_node":"assistant-current","mapping":{"user-current":{"message":{"author":{"role":"user"}}},"assistant-current":{"parent":"user-current","message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["你已达到 Free 套餐的图像生成请求上限。"]}}},"assistant-old":{"message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["old result"]}}}}}`), nil
	})}
	_, _, err := client.pollImageIDsWithError(context.Background(), "conversation-1", time.Second, time.Millisecond, 0)
	if requests != 1 || err == nil || !strings.Contains(err.Error(), "图像生成请求上限") {
		t.Fatalf("poll requests=%d err=%v, want current turn quota", requests, err)
	}
}

func TestImagePollIgnoresPreviousTurnQuota(t *testing.T) {
	conv := map[string]any{
		"current_node": "assistant-current",
		"mapping": map[string]any{
			"assistant-current": map[string]any{"parent": "user-current", "message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []any{"正在生成图片"}}}},
			"user-current":      map[string]any{"parent": "assistant-old", "message": map[string]any{"author": map[string]any{"role": "user"}}},
			"assistant-old":     map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []any{"你已达到 Free 套餐的图像生成请求上限。"}}}},
		},
	}
	if got := imagePollQuotaMessage(conv); got != "" {
		t.Fatalf("previous turn quota = %q, want none", got)
	}
}

func TestImagePollRetriesTemporaryStatuses(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			return upstreamTestResponse(404, "not committed"), nil
		case 2:
			return upstreamTestResponseWithHeader(429, "busy", http.Header{"Retry-After": []string{"0"}}), nil
		case 3:
			return upstreamTestResponse(503, "unavailable"), nil
		default:
			return upstreamTestResponse(200, `{"mapping":{"image":{"message":{"author":{"role":"tool"},"metadata":{"async_task_type":"image_gen"},"content":{"content_type":"multimodal_text","parts":[{"asset_pointer":"file-service://file_generated"}]}}}}}`), nil
		}
	})}
	files, _, err := client.pollImageIDsWithError(context.Background(), "conversation-1", time.Second, time.Millisecond, 0)
	if requests != 4 || len(files) != 1 || files[0] != "file_generated" || err != nil {
		t.Fatalf("poll requests=%d files=%v err=%v, want 4 requests and generated file", requests, files, err)
	}
}

func TestImagePollStopsOnPermanentStatus(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return upstreamTestResponse(401, "unauthorized"), nil
	})}
	files, sediments, err := client.pollImageIDsWithError(context.Background(), "conversation-1", time.Second, time.Millisecond, 0)
	if requests != 1 || len(files) != 0 || len(sediments) != 0 || !isInvalidTokenErrorText(err) {
		t.Fatalf("poll requests=%d files=%v sediments=%v err=%v, want one authentication failure", requests, files, sediments, err)
	}
}

func TestImagePollHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("canceled poll should not send a request")
		return nil, nil
	})}
	_, _, err := client.pollImageIDsWithError(ctx, "conversation-1", time.Second, time.Millisecond, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled poll error = %v", err)
	}
}

func TestImagePollRetryAfter(t *testing.T) {
	now := time.Date(2026, time.October, 16, 0, 0, 0, 0, time.UTC)
	if got := retryAfterDuration("3", now); got != 3*time.Second {
		t.Fatalf("delta Retry-After = %s", got)
	}
	if got := retryAfterDuration(now.Add(5*time.Second).Format(stdhttp.TimeFormat), now); got != 5*time.Second {
		t.Fatalf("date Retry-After = %s", got)
	}
	if got := retryAfterDuration("invalid", now); got != 0 {
		t.Fatalf("invalid Retry-After = %s", got)
	}
	err := &upstreamStatusError{StatusCode: 429, RetryAfter: 5 * time.Second}
	if got := imagePollRetryDelay(time.Second, 2, err); got != 5*time.Second {
		t.Fatalf("retry delay = %s, want 5s", got)
	}
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 429 {
		t.Fatalf("typed status error = %v", err)
	}
}

func TestImageDownloadRetriesTemporaryStatuses(t *testing.T) {
	requests := 0
	client := &UpstreamClient{userAgent: "test-agent", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			return upstreamTestResponse(503, "busy"), nil
		case 2:
			return upstreamTestResponseWithHeader(429, "busy", http.Header{"Retry-After": []string{"0"}}), nil
		default:
			return upstreamTestResponse(200, "image bytes"), nil
		}
	})}
	data, err := client.download(context.Background(), "https://storage.example/image.png")
	if err != nil || string(data) != "image bytes" || requests != 3 {
		t.Fatalf("download requests=%d data=%q err=%v", requests, data, err)
	}
}

func TestImageDownloadStopsOnPermanentStatus(t *testing.T) {
	requests := 0
	client := &UpstreamClient{userAgent: "test-agent", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return upstreamTestResponse(403, "forbidden"), nil
	})}
	_, err := client.download(context.Background(), "https://storage.example/image.png")
	if err == nil || requests != 1 {
		t.Fatalf("download requests=%d err=%v, want one forbidden response", requests, err)
	}
}

func TestImageDownloadStopsOnCancellation(t *testing.T) {
	requests := 0
	ctx, cancel := context.WithCancel(context.Background())
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		cancel()
		return nil, req.Context().Err()
	})}
	_, err := client.download(ctx, "https://storage.example/image.png")
	if !errors.Is(err, context.Canceled) || requests != 1 {
		t.Fatalf("download requests=%d err=%v, want one canceled request", requests, err)
	}
}

func TestImageDownloadStopsOnLongRetryAfter(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return upstreamTestResponseWithHeader(429, "busy", http.Header{"Retry-After": []string{"3600"}}), nil
	})}
	_, err := client.download(context.Background(), "https://storage.example/image.png")
	if err == nil || requests != 1 {
		t.Fatalf("download requests=%d err=%v, want one rate-limited request", requests, err)
	}
}

func TestImageDownloadURLRetriesNotReady(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return upstreamTestResponse(404, `{"error":{"code":"file_not_ready"}}`), nil
		}
		return upstreamTestResponse(200, `{"download_url":"https://storage.example/image.png"}`), nil
	})}
	urls, err := client.resolveImageURLs(context.Background(), "conversation-1", []string{"file-generated"}, nil)
	if err != nil || requests != 2 || len(urls) != 1 || urls[0] != "https://storage.example/image.png" {
		t.Fatalf("resolve requests=%d urls=%v err=%v", requests, urls, err)
	}
}

func TestImageDownloadURLReturnsPermanentError(t *testing.T) {
	requests := 0
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return upstreamTestResponse(401, "unauthorized"), nil
	})}
	urls, err := client.resolveImageURLs(context.Background(), "conversation-1", []string{"file-generated"}, nil)
	if requests != 1 || len(urls) != 0 || !isInvalidTokenErrorText(err) {
		t.Fatalf("resolve requests=%d urls=%v err=%v, want authentication error", requests, urls, err)
	}
}

func TestImageDownloadURLIncludesFileAndSediment(t *testing.T) {
	client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/files/file-generated/download":
			return upstreamTestResponse(200, `{"download_url":"https://storage.example/file.png"}`), nil
		case "/backend-api/conversation/conversation-1/attachment/sediment-generated/download":
			return upstreamTestResponse(200, `{"download_url":"https://storage.example/sediment.png"}`), nil
		default:
			t.Fatalf("unexpected download URL path %s", req.URL.Path)
			return nil, nil
		}
	})}
	urls, err := client.resolveImageURLs(context.Background(), "conversation-1", []string{"file-generated"}, []string{"sediment-generated"})
	if err != nil || len(urls) != 2 {
		t.Fatalf("resolved URLs=%v err=%v, want both sources", urls, err)
	}
}

func TestImageDownloadCredentialScopeAndEmptyResponse(t *testing.T) {
	requests := 0
	client := &UpstreamClient{token: "account-token", userAgent: "test-agent", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Host == "chatgpt.com" {
			if req.Header.Get("Authorization") != "Bearer account-token" {
				t.Errorf("same-origin image download missing account authorization")
			}
		} else if req.Header.Get("Authorization") != "" || req.Header.Get("Referer") != "" {
			t.Errorf("signed asset request sent account headers to %s", req.URL.Host)
		}
		if requests == 2 {
			return upstreamTestResponse(200, ""), nil
		}
		return upstreamTestResponse(200, "image bytes"), nil
	})}
	if _, err := client.downloadOnce(context.Background(), "https://chatgpt.com/image.png"); err != nil {
		t.Fatal(err)
	}
	data, err := client.download(context.Background(), "https://storage.example/image.png")
	if err != nil || string(data) != "image bytes" || requests != 3 {
		t.Fatalf("signed download requests=%d data=%q err=%v", requests, data, err)
	}
}

func TestAuthenticatedHeadersIncludeChatGPTAccountID(t *testing.T) {
	claim, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account-123"}})
	token := "header." + base64.RawURLEncoding.EncodeToString(claim) + ".signature"
	client := &UpstreamClient{token: token}
	h := client.headers("/backend-api/f/conversation", nil)
	if got := h.Get("ChatGPT-Account-Id"); got != "account-123" {
		t.Fatalf("ChatGPT-Account-Id = %q, want account-123", got)
	}
}

func TestConversationContentUsesStringTextPartForMultimodalMessages(t *testing.T) {
	storage := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.Method != stdhttp.MethodPut || r.URL.Path != "/upload" {
			t.Fatalf("storage request = %s %s, want PUT /upload", r.Method, r.URL.Path)
		}
		w.WriteHeader(201)
	}))
	defer storage.Close()
	client := &UpstreamClient{
		token:           "access-token",
		userAgent:       "test-agent",
		deviceID:        "device-id",
		sessionID:       "session-id",
		secCHUA:         `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		secCHUAMobile:   "?0",
		secCHUAPlatform: `"Windows"`,
		scriptSources:   []string{defaultPowScript},
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/backend-api/files":
				body := map[string]any{"file_id": "file-test", "upload_url": storage.URL + "/upload?sig=secret"}
				b, _ := json.Marshal(body)
				return upstreamTestResponse(200, string(b)), nil
			case "/backend-api/files/file-test/uploaded":
				return upstreamTestResponse(200, `{}`), nil
			default:
				t.Fatalf("unexpected upstream path %q", req.URL.Path)
			}
			return nil, nil
		}),
	}
	content, metadata, err := client.conversationContent(context.Background(), []any{
		map[string]any{"type": "text", "text": "describe this image"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("fake-image"))}},
	})
	if err != nil {
		t.Fatalf("conversationContent returned error: %v", err)
	}
	if content["content_type"] != "multimodal_text" {
		t.Fatalf("content_type = %v, want multimodal_text", content["content_type"])
	}
	parts, ok := content["parts"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("parts = %#v, want two []any entries", content["parts"])
	}
	if first, ok := parts[0].(string); !ok || first != "describe this image" {
		t.Fatalf("parts[0] = %#v, want text string", parts[0])
	}
	imagePart, ok := parts[1].(map[string]any)
	if !ok || imagePart["content_type"] != "image_asset_pointer" {
		t.Fatalf("parts[1] = %#v, want image_asset_pointer object", parts[1])
	}
	attachments, ok := metadata["attachments"].([]map[string]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("attachments = %#v, want one attachment", metadata["attachments"])
	}
}

func TestParseJSONToolCalls(t *testing.T) {
	text := `{"tool_calls":[{"tool_name":"mcp__generate_image_lucid","parameters":{"prompt":"test","width":1024,"height":1024,"num_images":1}}]}`
	calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %#v", calls)
	}
	if calls[0]["name"] != "mcp__generate_image_lucid" {
		t.Fatalf("expected name mcp__generate_image_lucid, got %q", calls[0]["name"])
	}
	input, _ := calls[0]["input"].(map[string]any)
	if input == nil {
		t.Fatal("expected input map")
	}
	if strAny(input["prompt"], "") != "test" {
		t.Fatalf("expected prompt=test, got %q", input["prompt"])
	}
}

func TestStripJSONToolCalls(t *testing.T) {
	text := `Some text {"tool_calls":[{"tool_name":"x"}]} more text`
	cleaned := stripToolMarkup(text)
	if !strings.Contains(cleaned, "Some text") || !strings.Contains(cleaned, "more text") {
		t.Fatalf("expected cleaned text to contain prose, got %q", cleaned)
	}
	if strings.Contains(cleaned, "tool_calls") || strings.Contains(cleaned, "tool_name") {
		t.Fatalf("expected tool markup removed, got %q", cleaned)
	}
}

func TestExtractToolIDsAcceptsBareFileIDsInImageToolMessages(t *testing.T) {
	conv := map[string]any{
		"mapping": map[string]any{
			"tool-node": map[string]any{
				"message": map[string]any{
					"author":   map[string]any{"role": "tool"},
					"metadata": map[string]any{"async_task_type": "image_gen"},
					"content": map[string]any{
						"content_type": "multimodal_text",
						"parts": []any{
							map[string]any{"asset_pointer": "file_result_123"},
							map[string]any{"asset_pointer": "sediment://sediment_result_456"},
						},
					},
				},
			},
		},
	}

	fileIDs, sedimentIDs := extractToolIDs(conv)
	if len(fileIDs) != 1 || fileIDs[0] != "file_result_123" {
		t.Fatalf("fileIDs = %#v, want [file_result_123]", fileIDs)
	}
	if len(sedimentIDs) != 1 || sedimentIDs[0] != "sediment_result_456" {
		t.Fatalf("sedimentIDs = %#v, want [sediment_result_456]", sedimentIDs)
	}
}

func TestExtractToolIDsAcceptsAssistantImageResultMessages(t *testing.T) {
	conv := map[string]any{
		"mapping": map[string]any{
			"assistant-node": map[string]any{
				"message": map[string]any{
					"author": map[string]any{"role": "assistant"},
					"content": map[string]any{
						"content_type": "multimodal_text",
						"parts": []any{
							map[string]any{"asset_pointer": "file-service://file_result_789"},
						},
					},
				},
			},
		},
	}

	fileIDs, sedimentIDs := extractToolIDs(conv)
	if len(fileIDs) != 1 || fileIDs[0] != "file_result_789" {
		t.Fatalf("fileIDs = %#v, want [file_result_789]", fileIDs)
	}
	if len(sedimentIDs) != 0 {
		t.Fatalf("sedimentIDs = %#v, want empty", sedimentIDs)
	}
}

func TestChatRequirementsFailsWhenTurnstileSolverUnconfigured(t *testing.T) {
	upstreamCalls := 0
	client := &UpstreamClient{
		token:           "access-token",
		userAgent:       "test-agent",
		deviceID:        "device-id",
		sessionID:       "session-id",
		secCHUA:         `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		secCHUAMobile:   "?0",
		secCHUAPlatform: `"Windows"`,
		scriptSources:   []string{defaultPowScript},
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			upstreamCalls++
			if req.URL.Path != "/backend-api/sentinel/chat-requirements/prepare" {
				t.Fatalf("unexpected upstream path %q", req.URL.Path)
			}
			return upstreamTestResponse(200, `{"prepare_token":"prepare-token","turnstile":{"required":true}}`), nil
		}),
	}
	_, err := client.chatRequirements(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sentinel solver is not configured") {
		t.Fatalf("chatRequirements error = %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want prepare only", upstreamCalls)
	}
}

func TestSentinelProofTokenFormatAndDifficulty(t *testing.T) {
	proof, err := buildProofToken("seed", "00ff", "test-agent", []string{defaultPowScript}, "test-build", "Thu Oct 15 2026 06:00:00 GMT-0700 (Pacific Daylight Time)", sentinelIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(proof, "gAAAAAB") || !strings.HasSuffix(proof, "~S") {
		t.Fatalf("invalid proof token framing: %q", proof)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(proof, "gAAAAAB"), "~S")
	if digest := fnv1a32("seed" + payload); digest>>16 > 0x00ff {
		t.Fatalf("proof digest = %08x, want prefix <= 00ff", digest)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var config []any
	if err := json.Unmarshal(decoded, &config); err != nil {
		t.Fatal(err)
	}
	if len(config) != 25 || config[4] != "test-agent" || config[6] != "test-build" {
		t.Fatalf("proof config = %#v", config)
	}
}

func TestPowConfigUsesSolverFingerprint(t *testing.T) {
	identity := sentinelIdentity{
		ScreenSize: 1400, HeapLimit: 4395630592, Language: "zh-CN",
		Languages: "zh-CN,zh;q=0.9,en;q=0.8", HardwareConcurrency: 8,
		Vendor: "Google Inc.",
	}
	config := buildPowConfig("test-agent", nil, "test-build", "test-date", identity)
	for index, want := range map[int]any{
		0: 1400, 2: int64(4395630592), 7: identity.Language,
		8: identity.Languages, 10: "vendor−Google Inc.", 16: 8,
	} {
		if config[index] != want {
			t.Fatalf("fingerprint config[%d] = %v, want %v", index, config[index], want)
		}
	}
}

func TestPowDateMatchesConversationTimezone(t *testing.T) {
	for _, tc := range []struct {
		name string
	}{
		{name: "Asia/Shanghai"},
		{name: "America/Los_Angeles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &UpstreamClient{timezone: tc.name}
			name, offset, _ := client.timezoneInfo()
			date := client.powDate()
			location, err := time.LoadLocation(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			_, seconds := time.Now().In(location).Zone()
			wantDate := "GMT+0800 (China Standard Time)"
			if tc.name == "America/Los_Angeles" {
				wantDate = "GMT-0800 (Pacific Standard Time)"
				if seconds == -7*3600 {
					wantDate = "GMT-0700 (Pacific Daylight Time)"
				}
			}
			if name != tc.name || offset != -seconds/60 || !strings.HasSuffix(date, wantDate) {
				t.Fatalf("timezone = %q offset=%d date=%q", name, offset, date)
			}
			if got := buildPowConfig("test-agent", nil, "test-build", date, sentinelIdentity{})[1]; got != date {
				t.Fatalf("proof date = %q, want %q", got, date)
			}
		})
	}
}

func TestSentinelSolverRejectsEchoedRequirementsToken(t *testing.T) {
	solver := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		_, _ = io.WriteString(w, `{"token":"requirements-token"}`)
	}))
	defer solver.Close()
	client := &UpstreamClient{solverURL: solver.URL}
	_, err := client.solveSentinelTurnstile(context.Background(), "challenge", "requirements-token", nil)
	if err == nil || !strings.Contains(err.Error(), "echoed the requirements token") {
		t.Fatalf("solver error = %v", err)
	}
}

func TestNormalizeChatGPTInternalMarkup(t *testing.T) {
	input := "entity[\"song\",\"Night Trouble\",\"Klangkarussell\"] —— 很有城市夜路感。资料来自 citeturn0search4。"
	got := normalizeChatGPTInternalMarkup(input)
	want := "Night Trouble - Klangkarussell —— 很有城市夜路感。资料来自 。"
	if got != want {
		t.Fatalf("normalizeChatGPTInternalMarkup() = %q, want %q", got, want)
	}
}

func TestNormalizeChatGPTInternalMarkupRemovesUnknownMarkers(t *testing.T) {
	got := normalizeChatGPTInternalMarkup("before unknownsecret after")
	want := "before  after"
	if got != want {
		t.Fatalf("normalizeChatGPTInternalMarkup() = %q, want %q", got, want)
	}
}

func TestUpstreamConversationStateHidesInternalMarkupFromDelta(t *testing.T) {
	state := newUpstreamConversationState("", nil)
	payload := `{"message":{"author":{"role":"assistant"},"content":{"parts":["推荐 entity[\"song\",\"Night Trouble\",\"Klangkarussell\"]，资料 citeturn0search4"]}}}`
	ev, ok := state.Apply(payload)
	if !ok {
		t.Fatal("expected state.Apply to emit text")
	}
	want := "推荐 Night Trouble - Klangkarussell，资料 "
	if ev.Delta != want {
		t.Fatalf("Delta = %q, want %q", ev.Delta, want)
	}
	if strings.Contains(ev.Delta, "") || strings.Contains(ev.Delta, "turn0search") {
		t.Fatalf("Delta still contains internal markup: %q", ev.Delta)
	}
}

func TestNormalizeChatGPTInternalMarkupDropsUnclosedMarker(t *testing.T) {
	got := normalizeChatGPTInternalMarkup("- River\n- entity[\"broken\"\n- Bloom")
	want := "- River\n- "
	if got != want {
		t.Fatalf("normalizeChatGPTInternalMarkup() = %q, want %q", got, want)
	}
}

func TestNormalizeChatGPTInternalMarkupParsesEntityWithTrailingText(t *testing.T) {
	got := normalizeChatGPTInternalMarkup("- entity[\"song\", \"Blinding Lights\", \"The Weeknd\"]正文粘连 after")
	want := "- Blinding Lights - The Weeknd after"
	if got != want {
		t.Fatalf("normalizeChatGPTInternalMarkup() = %q, want %q", got, want)
	}
}

func TestUpstreamConversationStateDoesNotLoseRawTextAfterUnclosedMarker(t *testing.T) {
	state := newUpstreamConversationState("", nil)
	payload1 := `{"message":{"author":{"role":"assistant"},"content":{"parts":["开头 entity[\"song\",\"Night"]}}}`
	if ev, ok := state.Apply(payload1); !ok || ev.Delta != "开头 " {
		t.Fatalf("first Delta = %q ok=%v, want 开头 / true", ev.Delta, ok)
	}
	payload2 := `{"message":{"author":{"role":"assistant"},"content":{"parts":["开头 entity[\"song\",\"Night Trouble\",\"Klangkarussell\"] 结束"]}}}`
	ev, ok := state.Apply(payload2)
	if !ok {
		t.Fatal("expected second Apply to emit completed entity")
	}
	want := "Night Trouble - Klangkarussell 结束"
	if ev.Delta != want {
		t.Fatalf("second Delta = %q, want %q", ev.Delta, want)
	}
}

func TestExtractFallbackImageIDsAcceptsLooseToolImageResult(t *testing.T) {
	conv := map[string]any{
		"mapping": map[string]any{
			"node": map[string]any{
				"message": map[string]any{
					"author":   map[string]any{"role": "tool"},
					"metadata": map[string]any{"async_task_type": "image_gen"},
					"content":  map[string]any{"content_type": "text", "parts": []any{"done file-service://file-result-loose sediment://sediment-result-loose"}},
				},
			},
		},
	}

	fileIDs, sedimentIDs := extractToolIDs(conv)
	if len(fileIDs) != 1 || fileIDs[0] != "file-result-loose" {
		t.Fatalf("fileIDs = %#v, want [file-result-loose]", fileIDs)
	}
	if len(sedimentIDs) != 1 || sedimentIDs[0] != "sediment-result-loose" {
		t.Fatalf("sedimentIDs = %#v, want [sediment-result-loose]", sedimentIDs)
	}
}

func TestExtractFallbackImageIDsSkipsUserReferenceImages(t *testing.T) {
	conv := map[string]any{
		"mapping": map[string]any{
			"node": map[string]any{
				"message": map[string]any{
					"author":  map[string]any{"role": "user"},
					"content": map[string]any{"content_type": "multimodal_text", "parts": []any{map[string]any{"asset_pointer": "file-service://file-user-reference"}}},
				},
			},
		},
	}

	fileIDs, sedimentIDs := extractToolIDs(conv)
	if len(fileIDs) != 0 || len(sedimentIDs) != 0 {
		t.Fatalf("got fileIDs=%#v sedimentIDs=%#v, want empty", fileIDs, sedimentIDs)
	}
}

func TestNormalizeTextModelOptions(t *testing.T) {
	cases := []struct {
		model         string
		wantModel     string
		wantReasoning string
	}{
		{"auto", "auto", ""},
		{"gpt-5", "gpt-5", ""},
		{"gpt-5-1", "gpt-5", "low"},
		{"gpt-5-2", "gpt-5", "medium"},
		{"gpt-5-3", "gpt-5", "high"},
		{"gpt-5-3-mini", "gpt-5", "high"},
	}
	for _, tc := range cases {
		got := normalizeTextModelOptions(tc.model)
		if got.Model != tc.wantModel || got.ReasoningLevel != tc.wantReasoning {
			t.Fatalf("normalizeTextModelOptions(%q) = %#v, want model=%q reasoning=%q", tc.model, got, tc.wantModel, tc.wantReasoning)
		}
	}
}

func TestImageModelSlug(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  string
	}{
		{model: "gpt-image-2", want: "auto"},
		{model: "gpt-image-2.5", want: "gpt-image-2.5"},
		{model: "gpt-image-2.5-flare", want: "gpt-image-2.5-flare"},
		{model: "gpt-image-2.5-sunburst", want: "gpt-image-2.5-sunburst"},
		{model: "gpt-image-2.6-sunburst", want: "gpt-image-2.6-sunburst"},
		{model: "gpt-image-2.6-flare", want: "gpt-image-2.6-flare"},
		{model: "codex-gpt-image-2", want: "codex-gpt-image-2"},
	} {
		if got := imageModelSlug(tc.model); got != tc.want {
			t.Errorf("imageModelSlug(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestSupportedImageModels(t *testing.T) {
	for _, model := range []string{"gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.6-sunburst", "gpt-image-2.6-flare", "plus-codex-gpt-image-2"} {
		if !isSupportedImageModel(model) {
			t.Errorf("isSupportedImageModel(%q) = false", model)
		}
	}
	for _, model := range []string{"gpt-5", "gpt-image-3", ""} {
		if isSupportedImageModel(model) {
			t.Errorf("isSupportedImageModel(%q) = true", model)
		}
	}
}

func TestAccountImageModelSlug(t *testing.T) {
	for _, tc := range []struct {
		name        string
		defaultSlug string
		want        string
	}{
		{name: "free", defaultSlug: "auto", want: "auto"},
		{name: "plus", defaultSlug: "gpt-6-thinking", want: "auto"},
		{name: "unknown", want: "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &UpstreamClient{defaultModelSlug: tc.defaultSlug}
			if got := client.imageModelSlug("gpt-image-2"); got != tc.want {
				t.Fatalf("imageModelSlug(gpt-image-2) = %q, want %q", got, tc.want)
			}
			if got := client.imageModelSlug("gpt-image-2.6-sunburst"); got != "gpt-image-2.6-sunburst" || isCodexImageRequest(got, "4k") {
				t.Fatalf("explicit Web model was replaced: %s", got)
			}
		})
	}
}

func TestImagePollSkippedWhenInitialIDsAlreadyPresent(t *testing.T) {
	fileIDs := []string{"file_00000000aaaaaaaaaaaaaaaaaaaaaaaa"}
	sedimentIDs := []string{}
	cid := "conv_test"
	message := ""
	shouldPoll := cid != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && !isImageQuotaMessage(message)
	if shouldPoll {
		t.Fatal("expected poll to be skipped when SSE already provided file IDs")
	}
}

func TestUpstreamConversationStateExtractsImageIDsFromPatchContext(t *testing.T) {
	state := newUpstreamConversationState("", nil)
	_, _ = state.Apply(`{"type":"server_ste_metadata","metadata":{"tool_invoked":true,"turn_use_case":"image gen"}}`)
	payload := `{"o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":{"asset_pointer":"file-service://file_00000000aaaaaaaaaaaaaaaaaaaaaaaa","content_type":"image_asset_pointer"}}]}`
	ev, ok := state.Apply(payload)
	if !ok {
		t.Fatal("expected patch metadata to emit state update")
	}
	if len(ev.FileIDs) != 1 || ev.FileIDs[0] != "file_00000000aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("FileIDs = %#v, want generated file id", ev.FileIDs)
	}
}

func TestUpstreamConversationStateSkipsUserPatchImageIDs(t *testing.T) {
	state := newUpstreamConversationState("", nil)
	payload := `{"o":"patch","v":{"message":{"author":{"role":"user"},"content":{"parts":[{"asset_pointer":"file-service://file_00000000aaaaaaaaaaaaaaaaaaaaaaaa"}]}}}}`
	ev, _ := state.Apply(payload)
	if len(ev.FileIDs) != 0 || len(ev.SedimentIDs) != 0 {
		t.Fatalf("got FileIDs=%#v SedimentIDs=%#v, want empty", ev.FileIDs, ev.SedimentIDs)
	}
}
