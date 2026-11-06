package app

import (
	"encoding/json"
	"sort"
	"strings"
)

// upstreamImageUsageFromSSE only accepts token usage objects from an upstream
// event. Quota and rate-limit metadata must not become billed token usage.
func upstreamImageUsageFromSSE(payload string) map[string]any {
	var event map[string]any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return nil
	}
	return findImageTokenUsage(event, 0)
}

func findImageTokenUsage(value any, depth int) map[string]any {
	if depth > 5 {
		return nil
	}
	switch item := value.(type) {
	case map[string]any:
		if usage, ok := item["usage"].(map[string]any); ok && hasImageTokenCount(usage) {
			return usage
		}
		if path := strings.TrimSpace(strAny(item["p"], "")); strings.HasSuffix(path, "/usage") {
			if usage, ok := item["v"].(map[string]any); ok && hasImageTokenCount(usage) {
				return usage
			}
		}
		for _, key := range []string{"response", "message", "metadata", "data", "v"} {
			if usage := findImageTokenUsage(item[key], depth+1); usage != nil {
				return usage
			}
		}
	case []any:
		for _, nested := range item {
			if usage := findImageTokenUsage(nested, depth+1); usage != nil {
				return usage
			}
		}
	}
	return nil
}

func hasImageTokenCount(usage map[string]any) bool {
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens"} {
		if value, ok := usage[key]; ok && intAny(value, -1) >= 0 {
			return true
		}
	}
	return false
}

// imageUsageFieldNamesFromSSE reports only field names. It never retains event
// values, which can contain conversation text or generated image data.
func imageUsageFieldNamesFromSSE(payload string) []string {
	if !strings.Contains(payload, "usage") && !strings.Contains(payload, "token") {
		return nil
	}
	var event any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return nil
	}
	fields := map[string]bool{}
	var visit func(any, int)
	visit = func(value any, depth int) {
		if depth > 12 {
			return
		}
		switch item := value.(type) {
		case map[string]any:
			for key, nested := range item {
				switch key {
				case "usage", "input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "token_count", "input_token_count", "output_token_count", "total_token_count":
					fields[key] = true
				}
				visit(nested, depth+1)
			}
		case []any:
			for _, nested := range item {
				visit(nested, depth+1)
			}
		}
	}
	visit(event, 0)
	result := make([]string, 0, len(fields))
	for key := range fields {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
