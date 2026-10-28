package app

import (
	"context"
	"encoding/json"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

func TestGenerateCodexImageRequest(t *testing.T) {
	client := &UpstreamClient{
		token: "test-token",
		client: upstreamDoerFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/backend-api/codex/responses" {
				t.Fatalf("request path = %q", req.URL.Path)
			}
			var body struct {
				Model string `json:"model"`
				Tools []struct {
					Action       string `json:"action"`
					Quality      string `json:"quality"`
					OutputFormat string `json:"output_format"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Model != codexResponsesModel || len(body.Tools) != 1 || body.Tools[0].Action != "generate" || body.Tools[0].Quality != "auto" || body.Tools[0].OutputFormat != "png" {
				t.Fatalf("unexpected Codex request: %+v", body)
			}
			return upstreamTestResponse(200, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}}\n\ndata: [DONE]\n\n"), nil
		}),
	}
	result, err := client.GenerateCodexImage(context.Background(), "A blue cup", "plus-codex-gpt-image-2", "1024x1024", nil)
	if err != nil || len(result) != 1 || string(result[0].Bytes) != "image" {
		t.Fatalf("Codex image result = %+v, err = %v", result, err)
	}
}

func TestCodexFailureMessageFromFailedResponse(t *testing.T) {
	event := map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"error": map[string]any{"message": "image tool rejected the request"},
		},
	}
	if got := codexFailureMessage(event); got != "image tool rejected the request" {
		t.Fatalf("failure message = %q", got)
	}
}
