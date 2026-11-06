package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

func testAnimatedGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.White, color.Black}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second.SetColorIndex(1, 1, 1)
	var output bytes.Buffer
	if err := gif.EncodeAll(&output, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestImagePrepareOptionalConduit(t *testing.T) {
	for _, tc := range []struct {
		body      string
		wantError bool
	}{
		{`{"status":"ok","conduit_token":""}`, false},
		{`{"status":"ok","conduit_token":null}`, false},
		{`{"status":"ok"}`, false},
		{`{"conduit_token":"token"}`, false},
		{`{"status":"error","message":"not allowed"}`, true},
		{`{"error":{"message":"model not found"}}`, true},
		{`{}`, true},
		{`null`, true},
		{`not json`, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
				return upstreamTestResponse(200, tc.body), nil
			})}
			_, err := client.prepareImage(context.Background(), "test", "gpt-image-2.6-sunburst", chatRequirements{})
			if (err != nil) != tc.wantError {
				t.Fatalf("prepare error=%v", err)
			}
		})
	}
}

func gifConversationFixture() map[string]any {
	var conversation map[string]any
	_ = json.Unmarshal([]byte(`{"current_node":"final","mapping":{
		"final":{"parent":"user","message":{"id":"message-gif","author":{"role":"assistant"},"content":{"parts":["[Download](sandbox:/mnt/data/red%20ball.gif)"]}}},
		"user":{"parent":"old","message":{"author":{"role":"user"},"content":{"parts":["sandbox:/mnt/data/user.gif"]}}},
		"old":{"message":{"id":"old-message","author":{"role":"assistant"},"content":{"parts":["sandbox:/mnt/data/old.gif"]}}},
		"other-branch":{"message":{"id":"other","author":{"role":"assistant"},"content":{"parts":["sandbox:/mnt/data/other.gif"]}}}
	}}`), &conversation)
	return conversation
}

func TestConversationGIFsUseCurrentTurn(t *testing.T) {
	got := conversationGIFs(gifConversationFixture())
	if len(got) != 1 || got[0].MessageID != "message-gif" || got[0].Path != "/mnt/data/red ball.gif" {
		t.Fatalf("GIF attachments=%+v", got)
	}
	if got := sandboxGIFPaths("sandbox:/mnt/data/../../secret.gif https://other.test/image.gif sandbox:/mnt/data/bad%zz.gif"); len(got) != 0 {
		t.Fatalf("invalid paths accepted: %v", got)
	}
}

func TestImage26GenerationReturnsGIFInsteadOfPreview(t *testing.T) {
	gifData := testAnimatedGIF(t)
	conversation, _ := json.Marshal(gifConversationFixture())
	started := false
	client := &UpstreamClient{token: "test-token", userAgent: "test-browser", client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			return upstreamTestResponse(200, "<html></html>"), nil
		case "/backend-api/sentinel/chat-requirements/prepare":
			return upstreamTestResponse(200, `{"prepare_token":"prepared"}`), nil
		case "/backend-api/sentinel/chat-requirements/finalize":
			return upstreamTestResponse(200, `{"token":"requirements"}`), nil
		case "/backend-api/f/conversation/prepare", "/backend-api/f/conversation":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != "gpt-image-2.6-sunburst" {
				t.Fatalf("upstream model=%v", body["model"])
			}
			if req.URL.Path == "/backend-api/f/conversation/prepare" {
				return upstreamTestResponse(200, `{"status":"ok","conduit_token":""}`), nil
			}
			started = true
			if req.Header.Get("X-Conduit-Token") != "" {
				t.Fatal("unexpected conduit header")
			}
			return upstreamTestResponse(200, "data: {\"conversation_id\":\"conv\",\"message\":{\"id\":\"message-gif\",\"author\":{\"role\":\"assistant\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"[GIF](sandbox:/mnt/data/red%20ball.gif)\"]}}}\n\ndata: [DONE]\n\n"), nil
		case "/backend-api/conversation/conv":
			return upstreamTestResponse(200, string(conversation)), nil
		case "/backend-api/conversation/conv/interpreter/download":
			if req.URL.Query().Get("message_id") != "message-gif" || req.URL.Query().Get("sandbox_path") != "/mnt/data/red ball.gif" {
				t.Fatalf("download query=%v", req.URL.Query())
			}
			return upstreamTestResponse(200, `{"download_url":"https://chatgpt.com/test-result.gif"}`), nil
		case "/test-result.gif":
			return upstreamTestResponse(200, string(gifData)), nil
		default:
			t.Fatalf("unexpected upstream request: %s", req.URL.Path)
			return nil, nil
		}
	})}
	results, err := client.GenerateImage(context.Background(), "make an animated GIF", "gpt-image-2.6-sunburst", "", "", nil, imageGenerationOptions{Timeout: time.Second})
	if err != nil || !started || len(results) != 1 || !bytes.Equal(results[0].Bytes, gifData) {
		t.Fatalf("GIF result: started=%t count=%d err=%v", started, len(results), err)
	}
}

func TestGIFDownloadDoesNotReturnStaticFallback(t *testing.T) {
	conversation, _ := json.Marshal(gifConversationFixture())
	for _, invalid := range []bool{false, true} {
		client := &UpstreamClient{client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/backend-api/conversation/conv":
				return upstreamTestResponse(200, string(conversation)), nil
			case "/backend-api/conversation/conv/interpreter/download":
				if !invalid {
					return upstreamTestResponse(403, `{"error":"expired"}`), nil
				}
				return upstreamTestResponse(200, `{"download_url":"https://chatgpt.com/result"}`), nil
			case "/result":
				return upstreamTestResponse(200, string(testPNG(t))), nil
			default:
				t.Fatalf("unexpected static fallback request: %s", req.URL.Path)
				return nil, nil
			}
		})}
		results, err := client.downloadConversationGIFs(context.Background(), "conv", "GIF", nil)
		if err == nil || len(results) != 0 {
			t.Fatalf("invalid GIF accepted: results=%d err=%v", len(results), err)
		}
	}
}

func TestAnimatedGIFStoragePreservesFrames(t *testing.T) {
	for _, mode := range []string{"local", "webdav", "r2"} {
		t.Run(mode, func(t *testing.T) {
			dav, _ := testWebDAV(t)
			initialMode := mode
			if mode == "r2" {
				initialMode = "local"
			}
			s := testImageStorageServer(t, initialMode, dav.URL)
			if mode == "r2" {
				r2, _ := testR2(t)
				settings := s.imageStorageSettings()
				settings.Mode = "r2"
				settings.R2Endpoint = r2.URL
				settings.R2Bucket = "test"
				settings.R2AccessKeyID = "key"
				settings.R2SecretKey = "secret"
				s.cfg.Extra["image_storage"] = settings
			}
			original := testAnimatedGIF(t)
			rel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", original)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(rel, ".gif") || !isImageFile(rel) {
				t.Fatalf("saved path=%s", rel)
			}
			data, err := s.readStoredImage(context.Background(), rel)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatalf("stored GIF changed: %v", err)
			}
			animation, err := gif.DecodeAll(bytes.NewReader(data))
			if err != nil || len(animation.Image) != 2 {
				t.Fatalf("animation lost: %v", err)
			}
			response := httptest.NewRecorder()
			s.handleStoredImage(response, httptest.NewRequest("GET", "/images/"+rel, nil))
			if response.Code != 200 || response.Header().Get("Content-Type") != "image/gif" || !bytes.Equal(response.Body.Bytes(), original) {
				t.Fatalf("GIF serving status=%d mime=%s", response.Code, response.Header().Get("Content-Type"))
			}
			items := s.listImages(httptest.NewRequest("GET", "/api/images", nil), "")["items"].([]map[string]any)
			if len(items) != 1 {
				t.Fatalf("GIF missing from image list: %v", items)
			}
		})
	}
}

func TestChatGIFDataURL(t *testing.T) {
	markdown := buildChatImageMarkdown([]map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(testAnimatedGIF(t))}})
	if !strings.Contains(markdown, "data:image/gif;base64,") {
		t.Fatalf("GIF MIME lost: %s", markdown)
	}
}
