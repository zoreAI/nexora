package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var sandboxGIFPattern = regexp.MustCompile(`(?i)sandbox:(/mnt/data/[^\s"'<>\)\]]+\.gif)(?:[\s"'<>\)\]]|$)`)

func sandboxGIFPaths(text string) []string {
	paths := []string{}
	for _, match := range sandboxGIFPattern.FindAllStringSubmatch(text, -1) {
		value, err := url.PathUnescape(match[1])
		if err != nil || !strings.HasPrefix(path.Clean(value), "/mnt/data/") {
			continue
		}
		paths = append(paths, path.Clean(value))
	}
	return unique(paths)
}

type sandboxGIF struct {
	MessageID string
	Path      string
}

func conversationGIFs(conversation map[string]any) []sandboxGIF {
	mapping, _ := conversation["mapping"].(map[string]any)
	nodeID := strAny(conversation["current_node"], "")
	visited, seen := map[string]bool{}, map[string]bool{}
	artifacts := []sandboxGIF{}
	for nodeID != "" && !visited[nodeID] {
		visited[nodeID] = true
		node, _ := mapping[nodeID].(map[string]any)
		message, _ := node["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		role := strAny(author["role"], "")
		if role == "user" {
			break
		}
		if role == "assistant" {
			content, _ := message["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			messageID := strAny(message["id"], "")
			for _, part := range parts {
				for _, filePath := range sandboxGIFPaths(strAny(part, "")) {
					if messageID != "" && !seen[filePath] {
						seen[filePath] = true
						artifacts = append(artifacts, sandboxGIF{MessageID: messageID, Path: filePath})
					}
				}
			}
		}
		nodeID = strAny(node["parent"], "")
	}
	return artifacts
}

func (c *UpstreamClient) downloadConversationGIFs(ctx context.Context, conversationID, prompt string, usage map[string]any) ([]upstreamImageResult, error) {
	conversation, err := c.getConversation(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read generated GIF attachments: %w", err)
	}
	artifacts := conversationGIFs(conversation)
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("generated GIF link has no downloadable attachment in the current turn")
	}
	results := make([]upstreamImageResult, 0, len(artifacts))
	for _, artifact := range artifacts {
		query := url.Values{"message_id": {artifact.MessageID}, "sandbox_path": {artifact.Path}}
		downloadPath := "/backend-api/conversation/" + url.PathEscape(conversationID) + "/interpreter/download?" + query.Encode()
		downloadURL, err := c.imageDownloadURL(ctx, downloadPath)
		if err != nil {
			return nil, fmt.Errorf("resolve generated GIF: %w", err)
		}
		data, err := c.download(ctx, downloadURL)
		if err != nil {
			return nil, fmt.Errorf("download generated GIF: %w", err)
		}
		if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "gif" {
			return nil, fmt.Errorf("generated GIF attachment is not a valid GIF")
		}
		results = append(results, upstreamImageResult{Bytes: data, RevisedPrompt: prompt, Usage: usage})
	}
	return results, nil
}

func imageFileExtension(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/gif":
		return ".gif"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
