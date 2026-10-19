package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestImageChatStreamKeepsConnectionOpenWhileGenerating(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := streamImageChatWithKeepalive(r.Context(), w, 10*time.Millisecond, func() ([]upstreamImageResult, error) {
			<-release
			return nil, nil
		})
		if err != nil {
			t.Errorf("generate: %v", err)
			return
		}
		fmt.Fprint(w, "data: done\n\n")
		flushSSE(w)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for _, want := range []string{": stream-open\n", "\n", ": keep-alive\n", "\n"} {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %q: %v", want, err)
		}
		if line != want {
			t.Fatalf("stream line = %q, want %q", line, want)
		}
	}
	close(release)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read final stream line: %v", err)
		}
		if line == "data: done\n" {
			break
		}
	}
}

func TestImageChatStreamStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	finished := make(chan error, 1)
	go func() {
		_, err := streamImageChatWithKeepalive(ctx, httptest.NewRecorder(), time.Hour, func() ([]upstreamImageResult, error) {
			<-release
			return nil, nil
		})
		finished <- err
	}()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
}
