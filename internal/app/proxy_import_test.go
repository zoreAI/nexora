package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProxyNodeImport(t *testing.T) {
	result, err := parseProxyNodeImport(proxyNodeImportRequest{
		ExistingURLs: []string{"http://user:pass@proxy.example:80"},
		Text: strings.Join([]string{
			"http://USER:pass@PROXY.EXAMPLE 5",
			"http://user:pass@proxy.example:00080 6",
			"socks5h://[::1]:1080 0",
			"https://Proxy.Example:443",
			"https://proxy.example/ 8",
			"http://other.example -1",
			"http://other.example 999999999999999999999999999999",
			"http://missing.example/path",
			"http://too-many.example 1 extra",
		}, "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AddedCount != 3 || result.DuplicateCount != 2 || result.InvalidCount != 4 {
		t.Fatalf("import counts = %+v", result)
	}
	if result.Nodes[0].URL != "http://USER:pass@proxy.example" || result.Nodes[0].ImageConcurrencyLimit != 5 {
		t.Fatalf("first node = %+v", result.Nodes[0])
	}
	if result.Nodes[1].URL != "socks5h://[::1]:1080" || result.Nodes[1].ImageConcurrencyLimit != 0 {
		t.Fatalf("IPv6 node = %+v", result.Nodes[1])
	}
	if result.Nodes[2].URL != "https://proxy.example" || result.Nodes[2].ImageConcurrencyLimit != defaultProxyImportImageConcurrency {
		t.Fatalf("default concurrency node = %+v", result.Nodes[2])
	}
	if result.InvalidItems[0].Line != 6 || result.InvalidItems[3].Line != 9 {
		t.Fatalf("invalid line numbers = %+v", result.InvalidItems)
	}
}

func TestProxyNodeImportLimits(t *testing.T) {
	if _, err := parseProxyNodeImport(proxyNodeImportRequest{Text: strings.Repeat("x", maxProxyImportTextBytes+1)}); err == nil {
		t.Fatal("accepted oversized text")
	}
	if _, err := parseProxyNodeImport(proxyNodeImportRequest{Text: strings.Repeat("http://proxy.example\n", maxProxyImportLines+1)}); err == nil {
		t.Fatal("accepted too many nodes")
	}
	if _, err := parseProxyNodeImport(proxyNodeImportRequest{ExistingURLs: make([]string, maxProxyImportLines+1)}); err == nil {
		t.Fatal("accepted too many existing nodes")
	}
	if _, err := normalizeProxyNodeImportURL("socks5://[::1]:"); err == nil {
		t.Fatal("accepted empty IPv6 port")
	}
	credentialURL := "http://user:pass,word;more@proxy.example:3129"
	if normalized, err := normalizeProxyNodeImportURL(credentialURL); err != nil || normalized != credentialURL {
		t.Fatalf("credential URL = %q, %v", normalized, err)
	}
}

func TestProxyNodeImportAcceptsMixedNodeFormats(t *testing.T) {
	result, err := parseProxyNodeImport(proxyNodeImportRequest{Text: strings.Join([]string{
		"http://127.0.0.1:7897",
		"socks://proxy.example:35086:user:pass",
		"socks5h://proxy.example:35087:user:pass",
		"socks5://user:pass@proxy.example:35086",
	}, "\n")})
	if err != nil {
		t.Fatal(err)
	}
	if result.AddedCount != 3 || result.DuplicateCount != 1 || result.InvalidCount != 0 {
		t.Fatalf("unexpected import counts: added=%d duplicates=%d invalid=%d", result.AddedCount, result.DuplicateCount, result.InvalidCount)
	}
	if result.Nodes[1].URL != "socks5://user:pass@proxy.example:35086" || result.Nodes[2].URL != "socks5h://user:pass@proxy.example:35087" {
		t.Fatal("SOCKS URLs were not normalized")
	}
}

func TestHandleProxyNodeImportRequiresAdminAndDoesNotPersist(t *testing.T) {
	s := &Server{cfg: Config{AuthKey: "test-key"}}
	request := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/proxy/nodes/import", strings.NewReader(`{"text":"http://proxy.example 7"}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		s.handleProxyNodeImport(response, req)
		return response
	}
	if response := request(""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}
	response := request("test-key")
	if response.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body = %s", response.Code, response.Body.String())
	}
	var result proxyNodeImportResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.AddedCount != 1 || result.Nodes[0].ImageConcurrencyLimit != 7 {
		t.Fatalf("import result = %+v", result)
	}
}
