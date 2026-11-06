package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func markupTestPNG(t *testing.T, width, height int, fill color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, fill)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestApplyImageMarkup(t *testing.T) {
	base := markupTestPNG(t, 2, 1, color.NRGBA{R: 255, A: 255})
	overlay := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	overlay.SetNRGBA(0, 0, color.NRGBA{B: 255, A: 255})
	var markup bytes.Buffer
	if err := png.Encode(&markup, overlay); err != nil {
		t.Fatal(err)
	}
	inputs, err := applyImageMarkup([][]byte{base}, markup.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(base, markupTestPNG(t, 2, 1, color.NRGBA{R: 255, A: 255})) {
		t.Fatal("source image changed")
	}
	annotated, err := png.Decode(bytes.NewReader(inputs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(annotated.At(0, 0)).(color.NRGBA); got.B != 255 || got.R != 0 {
		t.Fatalf("overlay pixel = %+v", got)
	}
	if got := color.NRGBAModel.Convert(annotated.At(1, 0)).(color.NRGBA); got.R != 255 || got.B != 0 {
		t.Fatalf("base pixel = %+v", got)
	}
}

func TestApplyImageMarkupRejectsInvalidImages(t *testing.T) {
	base := markupTestPNG(t, 2, 1, color.NRGBA{R: 255, A: 255})
	for _, tc := range []struct {
		name, want string
		markup     []byte
	}{
		{"not PNG", "valid PNG", []byte("not an image")},
		{"wrong dimensions", "dimensions must match", markupTestPNG(t, 1, 1, color.NRGBA{})},
		{"no transparent pixels", "transparent pixels", markupTestPNG(t, 2, 1, color.NRGBA{B: 255, A: 255})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := applyImageMarkup([][]byte{base}, tc.markup)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseImageEditMarkup(t *testing.T) {
	base := markupTestPNG(t, 1, 1, color.NRGBA{R: 255, A: 255})
	markup := markupTestPNG(t, 1, 1, color.NRGBA{})
	dataURL := func(data []byte) string { return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data) }
	body, _ := json.Marshal(map[string]any{"prompt": "edit", "image": dataURL(base), "markup": map[string]any{"image_url": dataURL(markup)}})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	parsed, err := parseImageEditRequest(r)
	if err != nil || !bytes.Equal(parsed.Markup, markup) {
		t.Fatalf("JSON markup parsed = %t, err = %v", bytes.Equal(parsed.Markup, markup), err)
	}
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	_ = w.WriteField("prompt", "edit")
	f, _ := w.CreateFormFile("image", "input.png")
	_, _ = f.Write(base)
	f, _ = w.CreateFormFile("markup", "markup.png")
	_, _ = f.Write(markup)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &form)
	r.Header.Set("Content-Type", w.FormDataContentType())
	parsed, err = parseImageEditRequest(r)
	if err != nil || !bytes.Equal(parsed.Markup, markup) {
		t.Fatalf("multipart markup parsed = %t, err = %v", bytes.Equal(parsed.Markup, markup), err)
	}
}

func TestParseImageEditMarkupMaskConflict(t *testing.T) {
	base := base64.StdEncoding.EncodeToString(markupTestPNG(t, 1, 1, color.NRGBA{A: 255}))
	for _, tc := range []struct{ name, body, want string }{
		{"both", `{"prompt":"edit","image":"` + base + `","markup":"` + base + `","mask":"` + base + `"}`, "cannot be combined"},
		{"mask only", `{"prompt":"edit","image":"` + base + `","mask":"` + base + `"}`, "mask is not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			_, err := parseImageEditRequest(r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	_ = w.WriteField("prompt", "edit")
	_ = w.WriteField("mask", "data:image/png;base64,"+base)
	f, err := w.CreateFormFile("markup", "markup.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(markupTestPNG(t, 1, 1, color.NRGBA{}))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &form)
	r.Header.Set("Content-Type", w.FormDataContentType())
	_, err = parseImageEditRequest(r)
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("multipart conflict error = %v", err)
	}
}
