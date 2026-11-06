package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type imageStorageSettings struct {
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode"`
	WebDAVURL     string `json:"webdav_url"`
	Username      string `json:"webdav_username"`
	Password      string `json:"webdav_password"`
	RootPath      string `json:"webdav_root_path"`
	PublicBaseURL string `json:"public_base_url"`
	R2Endpoint    string `json:"r2_endpoint"`
	R2Bucket      string `json:"r2_bucket"`
	R2AccessKeyID string `json:"r2_access_key_id"`
	R2SecretKey   string `json:"r2_secret_access_key"`
	R2Prefix      string `json:"r2_prefix"`
	R2PublicURL   string `json:"r2_public_url"`
}

type imageStorageEntry struct {
	Rel            string `json:"rel"`
	Size           int64  `json:"size"`
	CreatedAt      string `json:"created_at"`
	WebDAV         bool   `json:"webdav"`
	R2             bool   `json:"r2,omitempty"`
	R2Key          string `json:"r2_key,omitempty"`
	R2ThumbnailKey string `json:"r2_thumbnail_key,omitempty"`
}

func parseImageStorageSettings(raw any) (imageStorageSettings, error) {
	if settings, ok := raw.(imageStorageSettings); ok {
		return settings, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return imageStorageSettings{}, errors.New("image_storage must be an object")
	}
	settings := imageStorageSettings{
		Enabled:       boolAny(m["enabled"], false),
		Mode:          strings.ToLower(strings.TrimSpace(strAny(m["mode"], "local"))),
		WebDAVURL:     strings.TrimRight(strings.TrimSpace(strAny(m["webdav_url"], "")), "/"),
		Username:      strings.TrimSpace(strAny(m["webdav_username"], "")),
		Password:      strAny(m["webdav_password"], ""),
		RootPath:      strings.Trim(strings.TrimSpace(strAny(m["webdav_root_path"], "chatgpt2api/images")), "/"),
		PublicBaseURL: strings.TrimRight(strings.TrimSpace(strAny(m["public_base_url"], "")), "/"),
		R2Endpoint:    strings.TrimRight(strings.TrimSpace(strAny(m["r2_endpoint"], "")), "/"),
		R2Bucket:      strings.TrimSpace(strAny(m["r2_bucket"], "")),
		R2AccessKeyID: strings.TrimSpace(strAny(m["r2_access_key_id"], "")),
		R2SecretKey:   strAny(m["r2_secret_access_key"], ""),
		R2Prefix:      strings.Trim(strings.TrimSpace(strAny(m["r2_prefix"], "images")), "/"),
		R2PublicURL:   strings.TrimRight(strings.TrimSpace(strAny(m["r2_public_url"], "")), "/"),
	}
	if settings.Mode == "" {
		settings.Mode = "local"
	}
	if settings.Mode != "local" && settings.Mode != "webdav" && settings.Mode != "both" && settings.Mode != "r2" && settings.Mode != "r2_both" {
		return imageStorageSettings{}, errors.New("invalid image_storage mode")
	}
	if !settings.Enabled {
		settings.Mode = "local"
	}
	for _, segment := range strings.Split(settings.RootPath, "/") {
		if segment == "." || segment == ".." {
			return imageStorageSettings{}, errors.New("invalid WebDAV root path")
		}
	}
	for _, segment := range strings.Split(settings.R2Prefix, "/") {
		if segment == "." || segment == ".." {
			return imageStorageSettings{}, errors.New("invalid R2 prefix")
		}
	}
	if settings.Mode == "webdav" || settings.Mode == "both" {
		if err := validateStorageURL(settings.WebDAVURL); err != nil {
			return imageStorageSettings{}, err
		}
	}
	if settings.PublicBaseURL != "" {
		if err := validateStorageURL(settings.PublicBaseURL); err != nil {
			return imageStorageSettings{}, fmt.Errorf("invalid public base URL: %w", err)
		}
	}
	if settings.Mode == "r2" || settings.Mode == "r2_both" {
		if err := validateStorageURL(settings.R2Endpoint); err != nil {
			return imageStorageSettings{}, fmt.Errorf("invalid R2 endpoint: %w", err)
		}
		if settings.R2Bucket == "" || settings.R2AccessKeyID == "" || settings.R2SecretKey == "" || strings.ContainsAny(settings.R2Bucket, "/\\") {
			return imageStorageSettings{}, errors.New("R2 bucket, access key ID and secret access key are required")
		}
	}
	if settings.R2PublicURL != "" {
		if err := validateStorageURL(settings.R2PublicURL); err != nil {
			return imageStorageSettings{}, fmt.Errorf("invalid R2 public URL: %w", err)
		}
	}
	return settings, nil
}

func validateStorageURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("WebDAV URL must be an http(s) URL without credentials, query or fragment")
	}
	return nil
}

func (s *Server) imageStorageSettings() imageStorageSettings {
	if settings, err := parseImageStorageSettings(s.cfg.Extra["image_storage"]); err == nil {
		return settings
	}
	return imageStorageSettings{Mode: "local", RootPath: "chatgpt2api/images"}
}

func (s *Server) imageURL(baseURL, rel string) string {
	return s.imageURLForEntry(baseURL, rel, s.imageIndex()[rel])
}

func (s *Server) imageURLForEntry(baseURL, rel string, entry imageStorageEntry) string {
	settings := s.imageStorageSettings()
	if prefix := settings.R2PublicURL; entry.R2 && prefix != "" {
		return prefix + "/" + firstNonEmpty(entry.R2Key, settings.r2Key(rel))
	}
	if prefix := settings.PublicBaseURL; entry.WebDAV && prefix != "" {
		return prefix + "/" + rel
	}
	return strings.TrimRight(baseURL, "/") + "/images/" + rel
}

func (s *Server) imageThumbnailURLForEntry(baseURL, rel string, entry imageStorageEntry) string {
	if prefix := s.imageStorageSettings().R2PublicURL; entry.R2ThumbnailKey != "" && prefix != "" {
		return prefix + "/" + entry.R2ThumbnailKey
	}
	return strings.TrimRight(baseURL, "/") + "/image-thumbnails/" + rel
}

func (settings imageStorageSettings) r2Client() *s3.Client {
	return s3.New(s3.Options{
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider(settings.R2AccessKeyID, settings.R2SecretKey, ""),
		BaseEndpoint: aws.String(settings.R2Endpoint),
		UsePathStyle: true,
	})
}

func (settings imageStorageSettings) r2Key(rel string) string {
	return strings.Trim(settings.R2Prefix+"/"+rel, "/")
}

func (settings imageStorageSettings) r2ThumbnailKey(rel string) string {
	return settings.r2Key("thumbnails/" + rel + ".jpg")
}

func (settings imageStorageSettings) r2Put(ctx context.Context, rel string, payload []byte) error {
	contentType := mime.TypeByExtension(filepath.Ext(rel))
	_, err := settings.r2Client().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(settings.R2Bucket), Key: aws.String(settings.r2Key(rel)), Body: bytes.NewReader(payload), ContentType: aws.String(contentType)})
	return err
}

func (settings imageStorageSettings) r2Get(ctx context.Context, key string) ([]byte, error) {
	out, err := settings.r2Client().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(settings.R2Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, (64<<20)+1))
	if len(data) > 64<<20 {
		return nil, errors.New("R2 object exceeds 64 MiB")
	}
	return data, err
}

func (settings imageStorageSettings) r2Delete(ctx context.Context, key string) error {
	_, err := settings.r2Client().DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(settings.R2Bucket), Key: aws.String(key)})
	return err
}

func (s *Server) imageIndex() map[string]imageStorageEntry {
	s.imageStorageMu.Lock()
	defer s.imageStorageMu.Unlock()
	return s.store.LoadImageIndex()
}

func (s *Server) updateImageIndex(rel string, entry *imageStorageEntry) error {
	s.imageStorageMu.Lock()
	defer s.imageStorageMu.Unlock()
	items := s.store.LoadImageIndex()
	if entry == nil {
		delete(items, rel)
	} else {
		items[rel] = *entry
	}
	return s.store.SaveImageIndex(items)
}

func (settings imageStorageSettings) davURL(rel string) string {
	parts := []string{}
	for _, part := range strings.Split(strings.Trim(settings.RootPath+"/"+rel, "/"), "/") {
		if part != "" {
			parts = append(parts, url.PathEscape(part))
		}
	}
	return settings.WebDAVURL + "/" + strings.Join(parts, "/")
}

func (settings imageStorageSettings) davRequest(ctx context.Context, method, target string, payload []byte, contentType string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	if settings.Username != "" || settings.Password != "" {
		req.SetBasicAuth(settings.Username, settings.Password)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if len(data) > 64<<20 {
		return nil, resp.StatusCode, errors.New("WebDAV response exceeds 64 MiB")
	}
	return data, resp.StatusCode, err
}

func (settings imageStorageSettings) put(ctx context.Context, rel string, payload []byte) error {
	segments := strings.Split(strings.Trim(settings.RootPath+"/"+filepath.ToSlash(filepath.Dir(rel)), "/"), "/")
	current := strings.TrimRight(settings.WebDAVURL, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." {
			continue
		}
		current += "/" + url.PathEscape(segment)
		_, status, err := settings.davRequest(ctx, "MKCOL", current, nil, "")
		if err != nil {
			return err
		}
		if status != http.StatusCreated && status != http.StatusMethodNotAllowed && status != http.StatusOK {
			return fmt.Errorf("WebDAV MKCOL failed: HTTP %d", status)
		}
	}
	contentType := mime.TypeByExtension(filepath.Ext(rel))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, status, err := settings.davRequest(ctx, http.MethodPut, settings.davURL(rel), payload, contentType)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("WebDAV PUT failed: HTTP %d", status)
	}
	return nil
}

func (settings imageStorageSettings) get(ctx context.Context, rel string) ([]byte, error) {
	data, status, err := settings.davRequest(ctx, http.MethodGet, settings.davURL(rel), nil, "")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("WebDAV GET failed: HTTP %d", status)
	}
	return data, nil
}

func (settings imageStorageSettings) delete(ctx context.Context, rel string) error {
	_, status, err := settings.davRequest(ctx, http.MethodDelete, settings.davURL(rel), nil, "")
	if err != nil {
		return err
	}
	if status != http.StatusNotFound && (status < 200 || status >= 300) {
		return fmt.Errorf("WebDAV DELETE failed: HTTP %d", status)
	}
	return nil
}

func (s *Server) readStoredImage(ctx context.Context, rel string) ([]byte, error) {
	path, err := s.imagePath(rel)
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entry := s.imageIndex()[rel]
	if entry.R2 {
		settings := s.imageStorageSettings()
		data, err := settings.r2Get(ctx, firstNonEmpty(entry.R2Key, settings.r2Key(rel)))
		if err == nil || !entry.WebDAV {
			return data, err
		}
	}
	if !entry.WebDAV {
		return nil, os.ErrNotExist
	}
	return s.imageStorageSettings().get(ctx, rel)
}

func (s *Server) deleteStoredImage(ctx context.Context, rel string) error {
	entry := s.imageIndex()[rel]
	if entry.R2ThumbnailKey != "" {
		if err := s.imageStorageSettings().r2Delete(ctx, entry.R2ThumbnailKey); err != nil {
			return err
		}
	}
	if entry.R2 {
		settings := s.imageStorageSettings()
		if err := settings.r2Delete(ctx, firstNonEmpty(entry.R2Key, settings.r2Key(rel))); err != nil {
			return err
		}
	}
	if entry.WebDAV {
		if err := s.imageStorageSettings().delete(ctx, rel); err != nil {
			return err
		}
	}
	path, err := s.imagePath(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(s.thumbnailPath(rel))
	return s.updateImageIndex(rel, nil)
}

func (s *Server) handleStoredImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rel, err := safeImageRel(strings.TrimPrefix(r.URL.Path, "/images/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, _ := s.imagePath(rel)
	if _, err := os.Stat(path); err == nil {
		http.ServeFile(w, r, path)
		return
	}
	data, err := s.readStoredImage(r.Context(), rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "image storage unavailable", http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(rel)))
	http.ServeContent(w, r, filepath.Base(rel), time.Time{}, bytes.NewReader(data))
}

func (s *Server) handleImageStorageTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	settings := s.imageStorageSettings()
	if r.ContentLength > 0 {
		var body map[string]any
		if !readBody(w, r, &body) {
			return
		}
		var err error
		settings, err = parseImageStorageSettings(body)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	rel := fmt.Sprintf(".nexora-storage-test-%d.txt", time.Now().UnixNano())
	var err error
	payload := []byte("nexora storage test\n")
	if settings.Mode == "r2" || settings.Mode == "r2_both" {
		err = settings.r2Put(r.Context(), rel, payload)
		if err == nil {
			var data []byte
			data, err = settings.r2Get(r.Context(), settings.r2Key(rel))
			if err == nil && !bytes.Equal(data, payload) {
				err = errors.New("R2 read-back content mismatch")
			}
			deleteErr := settings.r2Delete(r.Context(), settings.r2Key(rel))
			if err == nil {
				err = deleteErr
			}
		}
	} else {
		if err = validateStorageURL(settings.WebDAVURL); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		err = settings.put(r.Context(), rel, payload)
		if err == nil {
			var data []byte
			data, err = settings.get(r.Context(), rel)
			if err == nil && !bytes.Equal(data, payload) {
				err = errors.New("WebDAV read-back content mismatch")
			}
			deleteErr := settings.delete(r.Context(), rel)
			if err == nil {
				err = deleteErr
			}
		}
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{"result": map[string]any{"ok": false, "error": err.Error()}})
		return
	}
	writeJSON(w, 200, map[string]any{"result": map[string]any{"ok": true, "status": 200}})
}

func (s *Server) handleImageStorageSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	settings := s.imageStorageSettings()
	if settings.Mode == "local" {
		writeErr(w, 400, "remote image storage is not enabled")
		return
	}
	result := map[string]int{"uploaded": 0, "skipped": 0, "failed": 0, "thumbnails_uploaded": 0, "thumbnails_failed": 0}
	indexed := s.imageIndex()
	uploaded := map[string]imageStorageEntry{}
	newUploads := map[string]bool{}
	_ = filepath.WalkDir(s.imagesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isImageFile(path) {
			return nil
		}
		rel, err := filepath.Rel(s.imagesDir, path)
		if err != nil {
			result["failed"]++
			return nil
		}
		rel = filepath.ToSlash(rel)
		if settings.Mode == "r2" || settings.Mode == "r2_both" {
			if indexed[rel].R2 {
				result["skipped"]++
				return nil
			}
		} else if indexed[rel].WebDAV {
			result["skipped"]++
			return nil
		}
		payload, err := os.ReadFile(path)
		if err == nil {
			if settings.Mode == "r2" || settings.Mode == "r2_both" {
				err = settings.r2Put(r.Context(), rel, payload)
			} else {
				err = settings.put(r.Context(), rel, payload)
			}
		}
		if err == nil {
			info, infoErr := d.Info()
			if infoErr != nil {
				err = infoErr
			} else {
				entry := indexed[rel]
				entry.Rel = rel
				entry.Size = int64(len(payload))
				entry.CreatedAt = info.ModTime().Format(time.RFC3339)
				entry.WebDAV = entry.WebDAV || settings.Mode == "webdav" || settings.Mode == "both"
				if settings.Mode == "r2" || settings.Mode == "r2_both" {
					entry.R2 = true
					entry.R2Key = settings.r2Key(rel)
				}
				uploaded[rel] = entry
				newUploads[rel] = true
			}
		}
		if err != nil {
			result["failed"]++
		} else {
			result["uploaded"]++
		}
		return nil
	})
	if settings.Mode == "r2" || settings.Mode == "r2_both" {
		for rel, entry := range indexed {
			if _, ok := uploaded[rel]; ok {
				continue
			}
			uploaded[rel] = entry
		}
		for rel, entry := range uploaded {
			if !entry.R2 || entry.R2ThumbnailKey != "" {
				delete(uploaded, rel)
				continue
			}
			thumbnail, err := os.ReadFile(s.thumbnailPath(rel))
			if err != nil {
				var original []byte
				if newUploads[rel] {
					original, err = settings.r2Get(r.Context(), entry.R2Key)
				} else {
					original, err = s.readStoredImage(r.Context(), rel)
				}
				if err == nil {
					thumbnail, err = encodeImageThumbnail(original)
				}
			}
			if err == nil {
				err = settings.r2Put(r.Context(), "thumbnails/"+rel+".jpg", thumbnail)
			}
			if err != nil {
				result["thumbnails_failed"]++
				if !newUploads[rel] {
					delete(uploaded, rel)
				}
				continue
			}
			entry.R2ThumbnailKey = settings.r2ThumbnailKey(rel)
			uploaded[rel] = entry
			result["thumbnails_uploaded"]++
		}
	}
	if len(uploaded) > 0 {
		s.imageStorageMu.Lock()
		current := s.store.LoadImageIndex()
		for rel, entry := range uploaded {
			current[rel] = entry
		}
		err := s.store.SaveImageIndex(current)
		s.imageStorageMu.Unlock()
		if err != nil {
			result["failed"] += result["uploaded"]
			result["uploaded"] = 0
			result["thumbnails_failed"] += result["thumbnails_uploaded"]
			result["thumbnails_uploaded"] = 0
		}
	}
	writeJSON(w, 200, map[string]any{"result": result})
}

func isImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	}
	return false
}
