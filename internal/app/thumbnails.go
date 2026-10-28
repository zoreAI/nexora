package app

import (
	"bytes"
	"image"
	"image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Server) thumbnailPath(rel string) string {
	return filepath.Join(s.dataDir, "image_thumbnails", filepath.FromSlash(rel)+".jpg")
}

func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request, rel string) {
	rel, err := safeImageRel(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	thumb := s.thumbnailPath(rel)
	if _, err := os.Stat(thumb); err == nil {
		http.ServeFile(w, r, thumb)
		return
	}
	entry := s.imageIndex()[rel]
	if entry.R2ThumbnailKey != "" {
		if data, err := s.imageStorageSettings().r2Get(r.Context(), entry.R2ThumbnailKey); err == nil {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
			return
		}
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
	thumbnail, err := encodeImageThumbnail(data)
	if err != nil {
		w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(rel)))
		_, _ = w.Write(data)
		return
	}
	_ = os.MkdirAll(filepath.Dir(thumb), 0755)
	if err := os.WriteFile(thumb, thumbnail, 0644); err != nil {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(thumbnail)
		return
	}
	http.ServeFile(w, r, thumb)
}

func encodeImageThumbnail(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	w0, h0 := bounds.Dx(), bounds.Dy()
	if w0 <= 0 || h0 <= 0 {
		return nil, image.ErrFormat
	}
	maxSide := 360
	nw, nh := w0, h0
	if w0 >= h0 && w0 > maxSide {
		nw = maxSide
		nh = h0 * maxSide / w0
	} else if h0 > w0 && h0 > maxSide {
		nh = maxSide
		nw = w0 * maxSide / h0
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			sx := bounds.Min.X + x*w0/nw
			sy := bounds.Min.Y + y*h0/nh
			dst.Set(x, y, img.At(sx, sy))
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
