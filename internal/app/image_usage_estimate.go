package app

import (
	"bytes"
	"image"
	"math"
	"strings"
	"sync"

	"github.com/tiktoken-go/tokenizer"
	_ "golang.org/x/image/webp"
)

// The Web image route does not report token usage. These patch rates mirror
// chatgpt2api's local proxy calculation, not GPT Image billing tokens.
const imageUsageEstimateMethod = "chatgpt2api_patch_proxy"

var (
	imagePromptCodecOnce sync.Once
	imagePromptCodec     tokenizer.Codec
)

func imagePromptTokens(prompt string) (int, string) {
	imagePromptCodecOnce.Do(func() {
		imagePromptCodec, _ = tokenizer.Get(tokenizer.O200kBase)
	})
	if imagePromptCodec != nil {
		ids, _, err := imagePromptCodec.Encode(prompt)
		if err == nil {
			return len(ids), "o200k_base"
		}
	}
	return approxTokens(prompt), "character_fallback"
}

func imagePatchCount(width, height float64) int {
	return int(math.Ceil(width/32) * math.Ceil(height/32))
}

func imageInputPatchTokens(width, height int) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	w, h := float64(width), float64(height)
	scale := math.Min(1, 2048/math.Max(w, h))
	w, h = w*scale, h*scale
	if imagePatchCount(w, h) > 1536 {
		shrink := math.Sqrt(32 * 32 * 1536 / (w * h))
		wUnits, hUnits := w*shrink/32, h*shrink/32
		adjusted := shrink * math.Min(math.Floor(wUnits)/wUnits, math.Floor(hUnits)/hUnits)
		w, h = w*adjusted, h*adjusted
	}
	return min(imagePatchCount(math.Max(1, w), math.Max(1, h)), 1536)
}

func imageOutputPatchTokens(width, height int, quality string) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	patches := imagePatchCount(float64(width), float64(height))
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		return (patches*17 + 63) / 64
	case "high", "hd":
		return (patches*65 + 15) / 16
	default:
		return (patches*33 + 31) / 32
	}
}

func imageBytesPatchTokens(data []byte, output bool) (int, bool) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, false
	}
	if output {
		// The upstream image tool currently sends quality=auto.
		return imageOutputPatchTokens(config.Width, config.Height, "auto"), true
	}
	return imageInputPatchTokens(config.Width, config.Height), true
}

func estimateImageUsage(prompt, model, size, resolution string, inputs [][]byte, output []byte) map[string]any {
	imageInputTokens := 0
	for _, input := range inputs {
		count, ok := imageBytesPatchTokens(input, false)
		if !ok {
			return nil
		}
		imageInputTokens += count
	}
	imageOutputTokens, ok := imageBytesPatchTokens(output, true)
	if !ok {
		return nil
	}
	if isCodexImageRequest(model, resolution) {
		prompt = buildImagePrompt(prompt, size)
	} else {
		prompt = buildImagePromptWithOptions(prompt, size, resolution)
	}
	textTokens, textMethod := imagePromptTokens(prompt)
	inputTokens := textTokens + imageInputTokens
	return map[string]any{
		"input_tokens":          inputTokens,
		"input_tokens_details":  map[string]any{"text_tokens": textTokens, "image_tokens": imageInputTokens},
		"output_tokens":         imageOutputTokens,
		"output_tokens_details": map[string]any{"image_tokens": imageOutputTokens},
		"total_tokens":          inputTokens + imageOutputTokens,
		"estimated":             true,
		"estimation_method":     imageUsageEstimateMethod,
		"text_tokenizer":        textMethod,
	}
}

func imageUsageOrEstimate(actual map[string]any, prompt, model, size, resolution string, inputs [][]byte, output []byte) map[string]any {
	if len(actual) > 0 {
		return actual
	}
	return estimateImageUsage(prompt, model, size, resolution, inputs, output)
}

func officialImageUsage(usage map[string]any) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	result := map[string]any{}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if value, ok := usage[key]; ok {
			result[key] = value
		}
	}
	for _, key := range []string{"input_tokens_details", "output_tokens_details"} {
		details, ok := usage[key].(map[string]any)
		if !ok {
			continue
		}
		filtered := map[string]any{}
		for _, field := range []string{"text_tokens", "image_tokens"} {
			if value, ok := details[field]; ok {
				filtered[field] = value
			}
		}
		if len(filtered) > 0 {
			result[key] = filtered
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
