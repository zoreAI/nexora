package app

import (
	"bytes"
	"image"
	"image/png"
	"reflect"
	"testing"
)

func TestAggregateImageUsagePreservesActualUpstreamValues(t *testing.T) {
	upstream := map[string]any{
		"input_tokens": 12, "output_tokens": 34, "total_tokens": 46,
		"input_tokens_details": map[string]any{"image_tokens": 8},
	}
	if got := aggregateImageUsage([]map[string]any{upstream}); !reflect.DeepEqual(got, upstream) {
		t.Fatalf("single-image usage changed: %+v", got)
	}
	if got := aggregateImageUsage([]map[string]any{upstream, nil}); got != nil {
		t.Fatalf("missing upstream usage was invented: %+v", got)
	}
	other := map[string]any{
		"input_tokens": 3, "output_tokens": 5, "total_tokens": 8,
		"input_tokens_details": map[string]any{"image_tokens": 2},
	}
	got := aggregateImageUsage([]map[string]any{upstream, other})
	if got["input_tokens"] != 15 || got["output_tokens"] != 39 || got["total_tokens"] != 54 {
		t.Fatalf("multiple-image usage = %+v", got)
	}
	details, _ := got["input_tokens_details"].(map[string]any)
	if details["image_tokens"] != 10 {
		t.Fatalf("multiple-image usage details = %+v", details)
	}
}

func TestUpstreamImageUsageFromSSE(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    int
	}{
		{"direct", `{"usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46}}`, 46},
		{"completed response", `{"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46}}}`, 46},
		{"conversation patch", `{"o":"patch","v":[{"p":"/message/metadata/usage","o":"replace","v":{"total_tokens":46}}]}`, 46},
		{"quota metadata", `{"metadata":{"usage":{"remaining":5,"limit":100}}}`, -1},
		{"invalid", `{`, -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := upstreamImageUsageFromSSE(test.payload)
			if test.want < 0 {
				if usage != nil {
					t.Fatalf("unexpected usage = %+v", usage)
				}
				return
			}
			if got := intAny(usage["total_tokens"], -1); got != test.want {
				t.Fatalf("total_tokens = %d, want %d", got, test.want)
			}
		})
	}
}

func TestImageUsageFieldNamesFromSSE(t *testing.T) {
	got := imageUsageFieldNamesFromSSE(`{"message":{"content":"private prompt","metadata":{"usage":{"input_tokens":12,"output_tokens":34}}}}`)
	want := []string{"input_tokens", "output_tokens", "usage"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage field names = %v, want %v", got, want)
	}
	if got := imageUsageFieldNamesFromSSE(`{"message":{"content":"usage tokens private prompt"}}`); len(got) != 0 {
		t.Fatalf("private content was reported as fields: %v", got)
	}
	if got := imageUsageFieldNamesFromSSE(`{"metadata":{"token_count":12}}`); !reflect.DeepEqual(got, []string{"token_count"}) {
		t.Fatalf("token count field not reported: %v", got)
	}
}

func TestImagePatchTokenEstimate(t *testing.T) {
	for _, test := range []struct {
		width, height int
		want          int
	}{
		{1024, 1024, 1024},
		{512, 512, 256},
		{4096, 512, 512},
		{4096, 4096, 1521},
	} {
		if got := imageInputPatchTokens(test.width, test.height); got != test.want {
			t.Fatalf("imageInputPatchTokens(%d, %d) = %d, want %d", test.width, test.height, got, test.want)
		}
	}
	for _, test := range []struct {
		quality string
		want    int
	}{
		{"low", 272}, {"auto", 1056}, {"high", 4160}, {"hd", 4160},
	} {
		if got := imageOutputPatchTokens(1024, 1024, test.quality); got != test.want {
			t.Fatalf("imageOutputPatchTokens(1024, 1024, %q) = %d, want %d", test.quality, got, test.want)
		}
	}
}

func TestImageUsageEstimateUsesActualOutputDimensions(t *testing.T) {
	encode := func(width, height int) []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	input := encode(1024, 1024)
	output := encode(512, 512)
	usage := imageUsageOrEstimate(nil, "hello", "gpt-image-2", "", "", [][]byte{input}, output)
	if usage["input_tokens"] != 1025 || usage["output_tokens"] != 264 || usage["total_tokens"] != 1289 {
		t.Fatalf("estimated usage = %+v", usage)
	}
	if usage["estimated"] != true || usage["text_tokenizer"] != "o200k_base" {
		t.Fatalf("missing estimate metadata: %+v", usage)
	}
	actual := map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5}
	if got := imageUsageOrEstimate(actual, "hello", "gpt-image-2", "", "", [][]byte{input}, output); !reflect.DeepEqual(got, actual) {
		t.Fatalf("upstream usage was replaced: %+v", got)
	}
}

func TestOfficialImageUsageExcludesEstimateMetadata(t *testing.T) {
	usage := map[string]any{
		"input_tokens": 3, "output_tokens": 4, "total_tokens": 7,
		"input_tokens_details": map[string]any{"text_tokens": 1, "image_tokens": 2, "cached_tokens": 99},
		"estimated":            true, "estimation_method": imageUsageEstimateMethod, "text_tokenizer": "o200k_base",
	}
	got := officialImageUsage(usage)
	if _, ok := got["estimated"]; ok {
		t.Fatalf("estimate metadata leaked into official usage: %+v", got)
	}
	if _, ok := got["estimation_method"]; ok {
		t.Fatalf("estimate method leaked into official usage: %+v", got)
	}
	if _, ok := got["text_tokenizer"]; ok {
		t.Fatalf("tokenizer leaked into official usage: %+v", got)
	}
	details, _ := got["input_tokens_details"].(map[string]any)
	if _, ok := details["cached_tokens"]; ok {
		t.Fatalf("nonstandard detail leaked into official usage: %+v", got)
	}
}
