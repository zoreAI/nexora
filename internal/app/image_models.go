package app

import "strings"

// gpt-image-2 uses the auto Web route. Versioned Web model slugs are sent
// unchanged so the upstream can select the requested model.
var webImageModels = []string{
	"gpt-image-2",
	"gpt-image-2.5",
	"gpt-image-2.5-flare",
	"gpt-image-2.5-sunburst",
	"gpt-image-2.6-sunburst",
	"gpt-image-2.6-flare",
}

func normalizeImageModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func isWebImageModel(model string) bool {
	slug := normalizeImageModel(model)
	for _, supported := range webImageModels {
		if slug == supported {
			return true
		}
	}
	return false
}

func isCodexImageModel(model string) bool {
	slug := normalizeImageModel(model)
	return slug == "codex-gpt-image-2" ||
		strings.HasSuffix(slug, "-codex-gpt-image-2")
}

func isSupportedImageModel(model string) bool {
	return isWebImageModel(model) || isCodexImageModel(model)
}
