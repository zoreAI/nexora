package app

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamImageSlotsContinuesAfterGenerationFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	attempts := 0
	firstFailure := errors.New("first slot failed")
	succeeded, err := streamImageSlots(recorder, context.Background(), "gpt-image-2", 1, 3,
		func() ([]upstreamImageResult, error) {
			attempts++
			if attempts == 1 {
				return nil, firstFailure
			}
			return []upstreamImageResult{{Bytes: []byte("image")}}, nil
		},
		func(upstreamImageResult) (map[string]any, error) {
			return map[string]any{"url": "/images/result.png"}, nil
		},
	)
	if succeeded != 2 || !errors.Is(err, firstFailure) || attempts != 3 {
		t.Fatalf("stream result: succeeded=%d err=%v attempts=%d", succeeded, err, attempts)
	}
	output := recorder.Body.String()
	if !strings.Contains(output, `"object":"image.generation.message"`) || !strings.Contains(output, `"index":0`) || strings.Count(output, `"object":"image.generation.result"`) != 2 || !strings.Contains(output, `"index":2`) {
		t.Fatalf("stream events = %s", output)
	}
}

func TestStreamImageSlotsStopsOnSaveFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	attempts := 0
	saveFailure := errors.New("disk full")
	succeeded, err := streamImageSlots(recorder, context.Background(), "gpt-image-2", 1, 3,
		func() ([]upstreamImageResult, error) {
			attempts++
			return []upstreamImageResult{{Bytes: []byte("image")}}, nil
		},
		func(upstreamImageResult) (map[string]any, error) {
			if attempts == 2 {
				return nil, saveFailure
			}
			return map[string]any{"url": "/images/result.png"}, nil
		},
	)
	if succeeded != 1 || !errors.Is(err, saveFailure) || attempts != 2 {
		t.Fatalf("stream result: succeeded=%d err=%v attempts=%d", succeeded, err, attempts)
	}
	output := recorder.Body.String()
	if strings.Count(output, `"object":"image.generation.result"`) != 1 || !strings.Contains(output, `"object":"image.generation.message"`) || strings.Contains(output, `"index":2`) {
		t.Fatalf("stream events = %s", output)
	}
}

func TestStreamImageSlotsIncludesUsage(t *testing.T) {
	recorder := httptest.NewRecorder()
	usage := map[string]any{"input_tokens": 1, "output_tokens": 2, "total_tokens": 3, "estimated": true}
	_, err := streamImageSlots(recorder, context.Background(), "gpt-image-2", 1, 1,
		func() ([]upstreamImageResult, error) {
			return []upstreamImageResult{{Bytes: []byte("image")}}, nil
		},
		func(upstreamImageResult) (map[string]any, error) {
			return map[string]any{"url": "/images/result.png"}, nil
		},
		func(upstreamImageResult) map[string]any { return usage },
	)
	if err != nil {
		t.Fatal(err)
	}
	if output := recorder.Body.String(); !strings.Contains(output, `"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}`) || !strings.Contains(output, `"usage_estimated":true`) || strings.Contains(output, `"usage":{"estimated"`) {
		t.Fatalf("stream usage missing: %s", output)
	}
}
