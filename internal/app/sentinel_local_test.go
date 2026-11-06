package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeSentinelLocalSolver struct {
	identity sentinelIdentity
	token    string
	err      error
}

func (f fakeSentinelLocalSolver) Identity(context.Context) (sentinelIdentity, error) {
	return f.identity, f.err
}

func (f fakeSentinelLocalSolver) Turnstile(context.Context, map[string]any, string, string) (string, error) {
	return f.token, f.err
}

func TestLocalSentinelIdentityAndTokenValidation(t *testing.T) {
	identity := sentinelIdentity{UserAgent: "Chrome/146", Timezone: "Asia/Shanghai", DateString: "Fri Oct 16 2026 12:00:00 GMT+0800 (China Standard Time)"}
	client := &UpstreamClient{localSolver: fakeSentinelLocalSolver{identity: identity, token: "valid-token"}}
	ua, err := client.sentinelUserAgent(context.Background())
	if err != nil || ua != identity.UserAgent || client.timezone != identity.Timezone || client.browserDate != identity.DateString {
		t.Fatalf("identity: ua=%q timezone=%q date=%q err=%v", ua, client.timezone, client.browserDate, err)
	}
	if token, err := client.solveSentinelTurnstile(context.Background(), "dx", "p", nil); err != nil || token != "valid-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	client.localSolver = fakeSentinelLocalSolver{token: "p"}
	if _, err := client.solveSentinelTurnstile(context.Background(), "dx", "p", nil); err == nil || !strings.Contains(err.Error(), "echoed") {
		t.Fatalf("echoed token error=%v", err)
	}
	client.localSolver = fakeSentinelLocalSolver{err: errors.New("solver unavailable")}
	if _, err := client.solveSentinelTurnstile(context.Background(), "dx", "p", nil); err == nil || !strings.Contains(err.Error(), "solver unavailable") {
		t.Fatalf("solver error=%v", err)
	}
}

func TestChromeSentinelStartsWithIsolatedProfile(t *testing.T) {
	if os.Getenv("NEXORA_TEST_CHROME") != "1" {
		t.Skip("set NEXORA_TEST_CHROME=1 for the local Chrome startup fixture")
	}
	solver := &browserSentinelSolver{userAgent: upstreamUserAgent()}
	defer solver.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := solver.ensurePage(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLocalSentinelHandshake(t *testing.T) {
	if os.Getenv("CHATGPT2API_LIVE_SENTINEL") != "1" {
		t.Skip("set CHATGPT2API_LIVE_SENTINEL=1 for an upstream handshake")
	}
	data, err := os.ReadFile("../../config.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Proxy string `json:"proxy"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	client, err := NewUpstreamClient("", config.Proxy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseSentinelSolvers()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	identity, err := client.localSolver.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Timezone == "" || identity.DateString == "" {
		t.Fatal("solver identity is incomplete")
	}
	t.Logf("fingerprint: timezone=%s screen=%d heap=%d language=%s languages=%s hardware=%d vendor=%s", identity.Timezone, identity.ScreenSize, identity.HeapLimit, identity.Language, identity.Languages, identity.HardwareConcurrency, identity.Vendor)
	requirements, err := client.chatRequirements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requirements.Token == "" {
		t.Fatal("empty finalized requirements token")
	}
	t.Logf("handshake succeeded: proof=%t turnstile=%t", requirements.ProofToken != "", requirements.TurnstileToken != "")
}

func TestLiveLocalSentinelConversation(t *testing.T) {
	email := os.Getenv("CHATGPT2API_LIVE_ACCOUNT_EMAIL")
	if email == "" {
		t.Skip("set CHATGPT2API_LIVE_ACCOUNT_EMAIL for an upstream conversation")
	}
	configData, err := os.ReadFile("../../config.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Proxy string `json:"proxy"`
	}
	if err := json.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}
	accountsData, err := os.ReadFile("../../data/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	var accounts []Account
	if err := json.Unmarshal(accountsData, &accounts); err != nil {
		t.Fatal(err)
	}
	var selected *Account
	for i := range accounts {
		if accounts[i].Email != nil && *accounts[i].Email == email {
			selected = &accounts[i]
			break
		}
	}
	if selected == nil || selected.AccessToken == "" {
		t.Fatal("requested test account was not found with an access token")
	}
	client, err := NewUpstreamClientForAccount(*selected, config.Proxy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseSentinelSolvers()
	if os.Getenv("CHATGPT2API_LIVE_ACCOUNT_DIAG") == "1" {
		diagCtx, diagCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer diagCancel()
		payload, err := client.getJSON(diagCtx, "GET", "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		accounts, _ := payload["accounts"].(map[string]any)
		defaultAccount, _ := accounts["default"].(map[string]any)
		account, _ := defaultAccount["account"].(map[string]any)
		defaultID := strAny(account["account_id"], "")
		t.Logf("account id: jwt_present=%t default_present=%t equal=%t plan=%s deactivated=%v trial=%v view_only=%v", chatGPTAccountID(selected.AccessToken) != "", defaultID != "", defaultID == chatGPTAccountID(selected.AccessToken), strAny(account["plan_type"], ""), account["is_deactivated"], account["is_trial"], account["is_view_only"])
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runImage := func() {
		t.Helper()
		imageCtx, imageCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer imageCancel()
		images, err := client.GenerateImage(imageCtx, "A small red square centered on a plain white background", "gpt-image-2", "1024x1024", "", nil, imageGenerationOptions{Timeout: 3 * time.Minute})
		if err != nil {
			t.Fatalf("image generation failed: %v", err)
		}
		if len(images) == 0 || len(images[0].Bytes) < 1000 {
			t.Fatalf("image result is empty: count=%d", len(images))
		}
		t.Logf("image generation returned %d image(s), first image bytes=%d", len(images), len(images[0].Bytes))
	}
	if os.Getenv("CHATGPT2API_LIVE_IMAGE_ONLY") == "1" {
		runImage()
		return
	}
	model := os.Getenv("CHATGPT2API_LIVE_MODEL")
	if model == "" {
		model = "auto"
	}
	stream := client.StreamTextConversationEvents
	if os.Getenv("CHATGPT2API_LIVE_CHAT_HISTORY") == "1" {
		stream = client.StreamChatConversationEvents
	}
	events, errs := stream(ctx, []map[string]any{{"role": "user", "content": "Reply with exactly: OK"}}, model, "")
	var content strings.Builder
	for event := range events {
		content.WriteString(event.Delta)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("conversation failed: %v", err)
		}
	}
	if !strings.Contains(strings.ToUpper(content.String()), "OK") {
		t.Fatalf("conversation did not return the requested acknowledgement: response_len=%d", content.Len())
	}
	t.Log("conversation returned requested acknowledgement")
	if os.Getenv("CHATGPT2API_LIVE_IMAGE") == "1" {
		runImage()
	}
}
