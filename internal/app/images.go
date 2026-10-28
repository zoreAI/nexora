package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) saveImage(r *http.Request, data []byte) (string, string, error) {
	return s.saveImageWithBaseURL(r.Context(), s.baseURL(r), data)
}

func (s *Server) saveImageWithBaseURL(ctx context.Context, baseURL string, data []byte) (string, string, error) {
	s.maybeCleanupOldImages()
	sum := md5.Sum(data)
	relDir := filepath.Join(time.Now().Format("2006"), time.Now().Format("01"), time.Now().Format("02"))
	name := fmt.Sprintf("%d_%x.png", time.Now().Unix(), sum)
	rel := filepath.ToSlash(filepath.Join(relDir, name))
	settings := s.imageStorageSettings()
	if settings.Mode == "local" || settings.Mode == "both" || settings.Mode == "r2_both" {
		path := filepath.Join(s.imagesDir, relDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return "", "", err
		}
	}
	webdav := settings.Mode == "webdav" || settings.Mode == "both"
	r2 := settings.Mode == "r2" || settings.Mode == "r2_both"
	if webdav {
		if err := settings.put(ctx, rel, data); err != nil {
			return "", "", err
		}
	}
	if r2 {
		if err := settings.r2Put(ctx, rel, data); err != nil {
			return "", "", err
		}
	}
	entry := imageStorageEntry{Rel: rel, Size: int64(len(data)), CreatedAt: time.Now().Format(time.RFC3339), WebDAV: webdav, R2: r2}
	if r2 {
		entry.R2Key = settings.r2Key(rel)
		if thumbnail, err := encodeImageThumbnail(data); err == nil {
			if err := settings.r2Put(ctx, "thumbnails/"+rel+".jpg", thumbnail); err == nil {
				entry.R2ThumbnailKey = settings.r2ThumbnailKey(rel)
			}
		}
	}
	if err := s.updateImageIndex(rel, &entry); err != nil {
		return "", "", err
	}
	return rel, s.imageURL(baseURL, rel), nil
}
func (s *Server) recordOwner(id *Identity, rel string) {
	if id == nil {
		return
	}
	_ = s.store.UpdateOwners(func(owners map[string]string) map[string]string {
		owners[rel] = id.ID
		return owners
	})
}
func (s *Server) recordPrompt(rel, prompt string, isEdit bool) {
	_ = s.store.UpdatePrompts(func(ps map[string]map[string]any) map[string]map[string]any {
		ps[rel] = map[string]any{"prompt": prompt, "is_edit": isEdit, "created_at": time.Now().Unix()}
		return ps
	})
}
func (s *Server) maybeCleanupOldImages() {
	s.imageCleanupMu.Lock()
	if time.Since(s.lastImageCleanup) < time.Hour {
		s.imageCleanupMu.Unlock()
		return
	}
	s.lastImageCleanup = time.Now()
	s.imageCleanupMu.Unlock()
	s.cleanupOldImages()
}
func (s *Server) cleanupOldImages() int {
	days := s.cfg.ImageRetentionDays
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	protected := map[string]bool{}
	if s.cfg.CleanupProtectGallery {
		for _, it := range s.store.LoadGallery() {
			if rel := relClean(it.ImageRel); rel != "" {
				protected[rel] = true
			}
		}
	}
	if s.cfg.CleanupProtectUserImages {
		for rel, owner := range s.store.LoadOwners() {
			owner = strings.ToLower(strings.TrimSpace(owner))
			if rel = relClean(rel); rel != "" && owner != "" && owner != "admin" && owner != "__admin__" {
				protected[rel] = true
			}
		}
	}
	removed := 0
	_ = filepath.WalkDir(s.imagesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil || st.ModTime().After(cutoff) {
			return nil
		}
		rel, err := filepath.Rel(s.imagesDir, path)
		if err == nil && protected[filepath.ToSlash(rel)] {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	// 清理空目录，深目录优先
	dirs := []string{}
	_ = filepath.WalkDir(s.imagesDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != s.imagesDir {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
	if removed > 0 && s.logSvc != nil {
		s.logSvc.add("system", "清理旧图片", map[string]any{"removed": removed, "retention_days": days})
	}
	return removed
}

func relFromURL(u string) string {
	if i := strings.Index(u, "/images/"); i >= 0 {
		return relClean(u[i+8:])
	}
	return relClean(u)
}

func safeImageRel(value string) (string, error) {
	rel := relClean(value)
	if rel == "" {
		return "", fmt.Errorf("image path is required")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid image path")
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if cleaned == "." || cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid image path")
	}
	return cleaned, nil
}

func (s *Server) imagePath(rel string) (string, error) {
	cleaned, err := safeImageRel(rel)
	if err != nil {
		return "", err
	}
	base := filepath.Clean(s.imagesDir)
	target := filepath.Clean(filepath.Join(base, filepath.FromSlash(cleaned)))
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid image path")
	}
	return target, nil
}

func (s *Server) listImages(r *http.Request, ownerFilter string) map[string]any {
	owners := s.store.LoadOwners()
	prompts := s.store.LoadPrompts()
	tags := s.store.LoadTags()
	items := []map[string]any{}
	indexed := s.imageIndex()
	seen := map[string]bool{}
	appendItem := func(rel string, size int64, createdAt string, local bool, entry imageStorageEntry) {
		owner := owners[rel]
		if ownerFilter != "" && ((ownerFilter == "__unowned__" && owner != "") || (ownerFilter != "__unowned__" && owner != ownerFilter)) {
			return
		}
		pr := prompts[rel]
		items = append(items, map[string]any{"rel": rel, "path": rel, "name": filepath.Base(rel), "date": createdAt[:10], "size": size, "url": s.imageURLForEntry(s.baseURL(r), rel, entry), "thumbnail_url": s.imageThumbnailURLForEntry(s.baseURL(r), rel, entry), "created_at": createdAt, "tags": tags[rel], "owner_id": owner, "prompt": strAny(pr["prompt"], ""), "storage": imageStorageMode(local, entry)})
	}
	filepath.WalkDir(s.imagesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isImageFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(s.imagesDir, path)
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		st, err := d.Info()
		if err == nil {
			appendItem(rel, st.Size(), st.ModTime().Format(time.RFC3339), true, indexed[rel])
		}
		return nil
	})
	for rel, entry := range indexed {
		if !seen[rel] && (entry.WebDAV || entry.R2) && isImageFile(rel) {
			createdAt := entry.CreatedAt
			if len(createdAt) < 10 {
				createdAt = time.Now().Format(time.RFC3339)
			}
			appendItem(rel, entry.Size, createdAt, false, entry)
		}
	}
	sort.Slice(items, func(i, j int) bool { return strAny(items[i]["created_at"], "") > strAny(items[j]["created_at"], "") })
	return map[string]any{"items": items}
}

func imageStorageMode(local bool, entry imageStorageEntry) string {
	if local && entry.R2 {
		return "r2_both"
	}
	if entry.R2 {
		return "r2"
	}
	if local && entry.WebDAV {
		return "both"
	}
	if entry.WebDAV {
		return "webdav"
	}
	return "local"
}
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, 200, s.listImages(r, r.URL.Query().Get("owner")))
}
func (s *Server) handleMyImages(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	owner := id.ID
	if id.Role == "admin" {
		owner = "admin"
	}
	writeJSON(w, 200, s.listImages(r, owner))
}
func (s *Server) handleImageOwners(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	owners := s.store.LoadOwners()
	counts := map[string]int{}
	for _, o := range owners {
		counts[o]++
	}
	items := []map[string]any{{"id": "__admin__", "name": "管理员", "deleted": false, "count": counts["admin"]}, {"id": "__unowned__", "name": "未归属", "deleted": false, "count": 0}}
	for _, k := range s.store.LoadAuthKeys() {
		if k.Role == "user" {
			items = append(items, map[string]any{"id": k.ID, "name": k.Name, "deleted": false, "count": counts[k.ID]})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) handleImageDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b struct {
		Paths []string `json:"paths"`
	}
	if !readBody(w, r, &b) {
		return
	}
	owners := s.store.LoadOwners()
	removed := 0
	for _, p := range b.Paths {
		rel, err := safeImageRel(p)
		if err != nil {
			continue
		}
		if id.Role != "admin" && owners[rel] != id.ID {
			continue
		}
		path, err := s.imagePath(rel)
		if err != nil {
			continue
		}
		_, localErr := os.Stat(path)
		entry := s.imageIndex()[rel]
		if os.IsNotExist(localErr) && !entry.WebDAV && !entry.R2 {
			continue
		}
		if err := s.deleteStoredImage(r.Context(), rel); err == nil {
			removed++
			delete(owners, rel)
		} else {
			_ = s.store.SaveOwners(owners)
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	_ = s.store.SaveOwners(owners)
	writeJSON(w, 200, map[string]any{"removed": removed})
}
func (s *Server) handleImageDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var b struct {
		Paths []string `json:"paths"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range b.Paths {
		rel, err := safeImageRel(p)
		if err != nil {
			continue
		}
		data, err := s.readStoredImage(r.Context(), rel)
		if err != nil {
			if !os.IsNotExist(err) {
				http.Error(w, "image storage unavailable", http.StatusBadGateway)
				return
			}
			continue
		}
		f, _ := zw.Create(filepath.Base(rel))
		_, _ = f.Write(data)
	}
	_ = zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=images.zip")
	_, _ = w.Write(buf.Bytes())
}
func (s *Server) handleImageDownloadSingle(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	rel, err := safeImageRel(strings.TrimPrefix(r.URL.Path, "/api/images/download/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.readStoredImage(r.Context(), rel)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "image storage unavailable", http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(rel))
	http.ServeContent(w, r, filepath.Base(rel), time.Time{}, bytes.NewReader(data))
}
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/image-thumbnails/")
	s.serveThumbnail(w, r, rel)
}
func (s *Server) handleImageTags(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	tags := s.store.LoadTags()
	if r.Method == http.MethodGet {
		set := map[string]bool{}
		for _, ts := range tags {
			for _, t := range ts {
				set[t] = true
			}
		}
		arr := []string{}
		for t := range set {
			arr = append(arr, t)
		}
		sort.Strings(arr)
		writeJSON(w, 200, map[string]any{"tags": arr})
		return
	}
	if r.Method == http.MethodPost {
		var b struct {
			Path string   `json:"path"`
			Tags []string `json:"tags"`
		}
		if !readBody(w, r, &b) {
			return
		}
		tags[relClean(b.Path)] = b.Tags
		_ = s.store.SaveTags(tags)
		writeJSON(w, 200, map[string]any{"ok": true, "tags": b.Tags})
		return
	}
	writeErr(w, 405, "method not allowed")
}
func (s *Server) handleImageTagDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	tag, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/images/tags/"))
	tags := s.store.LoadTags()
	n := 0
	for rel, ts := range tags {
		out := []string{}
		for _, t := range ts {
			if t == tag {
				n++
			} else {
				out = append(out, t)
			}
		}
		tags[rel] = out
	}
	_ = s.store.SaveTags(tags)
	writeJSON(w, 200, map[string]any{"ok": true, "removed_from": n})
}
