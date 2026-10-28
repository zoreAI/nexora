package app

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testWebDAV(t *testing.T) (*httptest.Server, map[string][]byte) {
	t.Helper()
	objects := map[string][]byte{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "user" || password != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r.Body)
			objects[r.URL.Path] = buf.Bytes()
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			data, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server, objects
}

func testImageStorageServer(t *testing.T, mode, davURL string) *Server {
	t.Helper()
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "data"))
	t.Cleanup(func() { _ = store.Close() })
	settings, err := parseImageStorageSettings(map[string]any{
		"enabled": true, "mode": mode, "webdav_url": davURL,
		"webdav_username": "user", "webdav_password": "pass", "webdav_root_path": "images",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{root: root, dataDir: filepath.Join(root, "data"), imagesDir: filepath.Join(root, "data", "images"), store: store, cfg: Config{AuthKey: "admin-key", Extra: map[string]any{"image_storage": settings}}}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestImageStorageWebDAVModes(t *testing.T) {
	for _, mode := range []string{"webdav", "both"} {
		t.Run(mode, func(t *testing.T) {
			dav, objects := testWebDAV(t)
			s := testImageStorageServer(t, mode, dav.URL)
			payload := testPNG(t)
			rel, imageURL, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", payload)
			if err != nil || imageURL != "http://app.test/images/"+rel {
				t.Fatalf("save: rel=%q url=%q err=%v", rel, imageURL, err)
			}
			if !bytes.Equal(objects["/images/"+rel], payload) || !s.imageIndex()[rel].WebDAV {
				t.Fatal("image or index was not stored remotely")
			}
			path, _ := s.imagePath(rel)
			_, statErr := os.Stat(path)
			if mode == "webdav" && !os.IsNotExist(statErr) || mode == "both" && statErr != nil {
				t.Fatalf("local image in %s mode: %v", mode, statErr)
			}
			if mode == "both" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest(http.MethodGet, "/images/"+rel, nil)
			response := httptest.NewRecorder()
			s.handleStoredImage(response, request)
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), payload) {
				t.Fatalf("remote read: %d %s", response.Code, response.Body.String())
			}
			items := s.listImages(request, "")["items"].([]map[string]any)
			if len(items) != 1 || items[0]["storage"] != "webdav" {
				t.Fatalf("remote list: %+v", items)
			}
			response = httptest.NewRecorder()
			s.serveThumbnail(response, httptest.NewRequest(http.MethodGet, "/image-thumbnails/"+rel, nil), rel)
			if response.Code != 200 || response.Body.Len() == 0 {
				t.Fatalf("remote thumbnail: %d", response.Code)
			}
			response = httptest.NewRecorder()
			download := httptest.NewRequest(http.MethodGet, "/api/images/download/"+rel, nil)
			download.Header.Set("Authorization", "Bearer admin-key")
			s.handleImageDownloadSingle(response, download)
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), payload) {
				t.Fatalf("remote download: %d", response.Code)
			}
			if err := s.deleteStoredImage(context.Background(), rel); err != nil {
				t.Fatal(err)
			}
			if _, ok := objects["/images/"+rel]; ok || len(s.imageIndex()) != 0 {
				t.Fatal("remote delete left image or index")
			}
		})
	}
}

func TestImageStorageSyncAndSettingsValidation(t *testing.T) {
	dav, objects := testWebDAV(t)
	s := testImageStorageServer(t, "local", dav.URL)
	payload := testPNG(t)
	rel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", payload)
	if err != nil {
		t.Fatal(err)
	}
	settings := s.imageStorageSettings()
	settings.Mode = "both"
	s.cfg.Extra["image_storage"] = settings
	req := httptest.NewRequest(http.MethodPost, "/api/image-storage/sync", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer admin-key")
	response := httptest.NewRecorder()
	s.handleImageStorageSync(response, req)
	var result struct {
		Result map[string]int `json:"result"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Result["uploaded"] != 1 {
		t.Fatalf("sync: %d %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(objects["/images/"+rel], payload) {
		t.Fatal("sync did not upload existing local image")
	}
	if _, err := parseImageStorageSettings(map[string]any{"enabled": true, "mode": "webdav", "webdav_url": "file:///tmp/images"}); err == nil {
		t.Fatal("accepted invalid WebDAV URL")
	}
	if _, err := parseImageStorageSettings(map[string]any{"enabled": true, "mode": "both", "webdav_url": dav.URL, "webdav_root_path": "../images"}); err == nil {
		t.Fatal("accepted traversal in WebDAV root")
	}
}

func TestImageStorageSettingsAndConnectionEndpoint(t *testing.T) {
	dav, objects := testWebDAV(t)
	s := testImageStorageServer(t, "local", dav.URL)
	settings := map[string]any{
		"enabled": true, "mode": "webdav", "webdav_url": dav.URL,
		"webdav_username": "user", "webdav_password": "pass", "webdav_root_path": "images",
	}
	body, _ := json.Marshal(map[string]any{"image_storage": settings})
	req := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-key")
	response := httptest.NewRecorder()
	s.handleSettings(response, req)
	if response.Code != 200 || s.imageStorageSettings().Mode != "webdav" {
		t.Fatalf("save settings: %d %s", response.Code, response.Body.String())
	}
	stored := readJSONFile(filepath.Join(s.root, "config.json"), map[string]any{})
	if stored["image_storage"] == nil {
		t.Fatal("image storage settings were not persisted")
	}
	requestBody, _ := json.Marshal(settings)
	req = httptest.NewRequest(http.MethodPost, "/api/image-storage/test", bytes.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer admin-key")
	response = httptest.NewRecorder()
	s.handleImageStorageTest(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || len(objects) != 0 {
		t.Fatalf("test connection: %d %s objects=%d", response.Code, response.Body.String(), len(objects))
	}
}

func TestImageStorageRejectsInvalidStartupConfig(t *testing.T) {
	root := t.TempDir()
	config := map[string]any{
		"auth-key": "admin-key",
		"image_storage": map[string]any{
			"enabled": true, "mode": "webdav", "webdav_url": "file:///tmp/images",
		},
	}
	if err := writeJSONFile(filepath.Join(root, "config.json"), config); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(root); err == nil || !strings.Contains(err.Error(), "image_storage") {
		t.Fatalf("invalid storage config should prevent startup: %v", err)
	}
}

func TestImageStorageRemoteDeleteFailurePreservesLocalImage(t *testing.T) {
	dav, _ := testWebDAV(t)
	s := testImageStorageServer(t, "both", dav.URL)
	rel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", testPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	settings := s.imageStorageSettings()
	settings.Password = "wrong-password"
	s.cfg.Extra["image_storage"] = settings
	if err := s.deleteStoredImage(context.Background(), rel); err == nil {
		t.Fatal("remote deletion unexpectedly succeeded")
	}
	path, err := s.imagePath(rel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil || !s.imageIndex()[rel].WebDAV {
		t.Fatalf("local image or remote index was lost: %v", err)
	}
}

func TestImageStoragePublicURLOnlyForRemoteImages(t *testing.T) {
	dav, _ := testWebDAV(t)
	s := testImageStorageServer(t, "local", dav.URL)
	settings := s.imageStorageSettings()
	settings.PublicBaseURL = "https://cdn.example.com/images"
	s.cfg.Extra["image_storage"] = settings
	rel, imageURL, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", testPNG(t))
	if err != nil || imageURL != "http://app.test/images/"+rel {
		t.Fatalf("local image URL: %q, error: %v", imageURL, err)
	}
	settings.Mode = "both"
	s.cfg.Extra["image_storage"] = settings
	if got := s.imageURL("http://app.test", rel); got != imageURL {
		t.Fatalf("unsynced local image URL changed: %q", got)
	}
	remoteRel, remoteURL, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", testPNG(t))
	if err != nil || remoteURL != "https://cdn.example.com/images/"+remoteRel {
		t.Fatalf("remote image URL: %q, error: %v", remoteURL, err)
	}
}

func testR2(t *testing.T) (*httptest.Server, map[string][]byte) {
	t.Helper()
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = body
		case http.MethodGet:
			body, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	return server, objects
}

func TestImageStorageR2Lifecycle(t *testing.T) {
	for _, mode := range []string{"r2", "r2_both"} {
		t.Run(mode, func(t *testing.T) {
			r2, objects := testR2(t)
			s := testImageStorageServer(t, "local", "")
			settings := s.imageStorageSettings()
			settings.Mode = mode
			settings.R2Endpoint = r2.URL
			settings.R2Bucket = "test-bucket"
			settings.R2AccessKeyID = "key"
			settings.R2SecretKey = "secret"
			settings.R2Prefix = "images"
			settings.R2PublicURL = "https://cdn.example.test"
			s.cfg.Extra["image_storage"] = settings
			payload := testPNG(t)
			rel, imageURL, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", payload)
			if err != nil {
				t.Fatal(err)
			}
			entry := s.imageIndex()[rel]
			if !bytes.Equal(objects["images/"+rel], payload) || !entry.R2 || imageURL != "https://cdn.example.test/images/"+rel {
				t.Fatalf("R2 upload or index failed: %q", imageURL)
			}
			if entry.R2ThumbnailKey != "images/thumbnails/"+rel+".jpg" || len(objects[entry.R2ThumbnailKey]) == 0 {
				t.Fatalf("R2 thumbnail was not stored: %+v", entry)
			}
			path, _ := s.imagePath(rel)
			if mode == "r2_both" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			data, err := s.readStoredImage(context.Background(), rel)
			if err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("R2 read: %v", err)
			}
			items := s.listImages(httptest.NewRequest(http.MethodGet, "/api/images", nil), "")["items"].([]map[string]any)
			if len(items) != 1 || items[0]["storage"] != "r2" {
				t.Fatalf("R2 list: %+v", items)
			}
			if items[0]["thumbnail_url"] != "https://cdn.example.test/"+entry.R2ThumbnailKey {
				t.Fatalf("R2 thumbnail URL: %v", items[0]["thumbnail_url"])
			}
			response := httptest.NewRecorder()
			s.serveThumbnail(response, httptest.NewRequest(http.MethodGet, "/image-thumbnails/"+rel, nil), rel)
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), objects[entry.R2ThumbnailKey]) {
				t.Fatalf("R2 thumbnail read: %d", response.Code)
			}
			if err := s.deleteStoredImage(context.Background(), rel); err != nil || len(objects) != 0 || len(s.imageIndex()) != 0 {
				t.Fatalf("R2 delete: %v", err)
			}
		})
	}
}

func TestImageStorageR2Validation(t *testing.T) {
	if _, err := parseImageStorageSettings(map[string]any{"enabled": true, "mode": "r2", "r2_endpoint": "https://example.com"}); err == nil {
		t.Fatal("accepted R2 without bucket and credentials")
	}
	if _, err := parseImageStorageSettings(map[string]any{"enabled": true, "mode": "r2", "r2_endpoint": "https://example.com", "r2_bucket": "bucket", "r2_access_key_id": "key", "r2_secret_access_key": "secret", "r2_prefix": "../images"}); err == nil {
		t.Fatal("accepted R2 path traversal")
	}
}

func TestImageStorageR2ConnectionAndSync(t *testing.T) {
	r2, objects := testR2(t)
	s := testImageStorageServer(t, "local", "")
	payload := testPNG(t)
	rel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", payload)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{
		"enabled": true, "mode": "r2_both", "r2_endpoint": r2.URL,
		"r2_bucket": "test-bucket", "r2_access_key_id": "key",
		"r2_secret_access_key": "secret", "r2_prefix": "images",
	}
	body, _ := json.Marshal(settings)
	req := httptest.NewRequest(http.MethodPost, "/api/image-storage/test", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-key")
	response := httptest.NewRecorder()
	s.handleImageStorageTest(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || len(objects) != 0 {
		t.Fatalf("R2 connection test: %d %s", response.Code, response.Body.String())
	}
	parsed, err := parseImageStorageSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Extra["image_storage"] = parsed
	req = httptest.NewRequest(http.MethodPost, "/api/image-storage/sync", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer admin-key")
	response = httptest.NewRecorder()
	s.handleImageStorageSync(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"uploaded":1`) || !strings.Contains(response.Body.String(), `"thumbnails_uploaded":1`) || !bytes.Equal(objects["images/"+rel], payload) || len(objects[s.imageIndex()[rel].R2ThumbnailKey]) == 0 {
		t.Fatalf("R2 history sync: %d %s", response.Code, response.Body.String())
	}
}

func TestImageStorageR2SyncMissingThumbnail(t *testing.T) {
	r2, objects := testR2(t)
	s := testImageStorageServer(t, "local", "")
	settings := s.imageStorageSettings()
	settings.Mode = "r2"
	settings.R2Endpoint = r2.URL
	settings.R2Bucket = "test-bucket"
	settings.R2AccessKeyID = "key"
	settings.R2SecretKey = "secret"
	settings.R2Prefix = "images"
	s.cfg.Extra["image_storage"] = settings
	rel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", testPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	entry := s.imageIndex()[rel]
	delete(objects, entry.R2ThumbnailKey)
	entry.R2ThumbnailKey = ""
	if err := s.updateImageIndex(rel, &entry); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/image-storage/sync", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer admin-key")
	response := httptest.NewRecorder()
	s.handleImageStorageSync(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"thumbnails_uploaded":1`) || len(objects[s.imageIndex()[rel].R2ThumbnailKey]) == 0 {
		t.Fatalf("R2 thumbnail backfill: %d %s", response.Code, response.Body.String())
	}
}

func TestImageStorageR2KeepsOldWebDAVImages(t *testing.T) {
	dav, _ := testWebDAV(t)
	r2, objects := testR2(t)
	s := testImageStorageServer(t, "webdav", dav.URL)
	oldRel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", testPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	settings := s.imageStorageSettings()
	settings.Mode = "r2"
	settings.R2Endpoint = r2.URL
	settings.R2Bucket = "test-bucket"
	settings.R2AccessKeyID = "key"
	settings.R2SecretKey = "secret"
	settings.R2Prefix = "images"
	s.cfg.Extra["image_storage"] = settings
	if _, err := s.readStoredImage(context.Background(), oldRel); err != nil {
		t.Fatalf("old WebDAV image after switching to R2: %v", err)
	}
	newRel, _, err := s.saveImageWithBaseURL(context.Background(), "http://app.test", append(testPNG(t), 0))
	if err != nil || !s.imageIndex()[newRel].R2 || len(objects) == 0 {
		t.Fatalf("new R2 image after switch: %v", err)
	}
	settings.R2Prefix = "new-prefix"
	s.cfg.Extra["image_storage"] = settings
	if _, err := s.readStoredImage(context.Background(), newRel); err != nil {
		t.Fatalf("R2 image after changing prefix: %v", err)
	}
}
