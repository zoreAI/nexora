package app

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
)

const maxMarkupPixels = 16_000_000

// ChatGPT Web has no native markup field. Prepare an annotated first reference
// image so that the Web image tool can see the user's edit instructions.
func applyImageMarkup(inputs [][]byte, markup []byte) ([][]byte, error) {
	if len(markup) == 0 {
		return inputs, nil
	}
	if len(inputs) == 0 {
		return nil, errors.New("markup requires an input image")
	}
	baseConfig, _, err := image.DecodeConfig(bytes.NewReader(inputs[0]))
	if err != nil {
		return nil, fmt.Errorf("decode first image for markup: %w", err)
	}
	markupConfig, err := png.DecodeConfig(bytes.NewReader(markup))
	if err != nil {
		return nil, errors.New("markup must be a valid PNG image")
	}
	if baseConfig.Width <= 0 || baseConfig.Height <= 0 ||
		int64(baseConfig.Width)*int64(baseConfig.Height) > maxMarkupPixels {
		return nil, errors.New("first image is too large for markup")
	}
	if baseConfig.Width != markupConfig.Width || baseConfig.Height != markupConfig.Height {
		return nil, errors.New("markup dimensions must match the first image")
	}
	base, _, err := image.Decode(bytes.NewReader(inputs[0]))
	if err != nil {
		return nil, fmt.Errorf("decode first image for markup: %w", err)
	}
	overlay, err := png.Decode(bytes.NewReader(markup))
	if err != nil {
		return nil, errors.New("markup must be a valid PNG image")
	}
	transparent := false
	for y := overlay.Bounds().Min.Y; y < overlay.Bounds().Max.Y && !transparent; y++ {
		for x := overlay.Bounds().Min.X; x < overlay.Bounds().Max.X; x++ {
			_, _, _, alpha := overlay.At(x, y).RGBA()
			if alpha < 0xffff {
				transparent = true
				break
			}
		}
	}
	if !transparent {
		return nil, errors.New("markup PNG must contain transparent pixels")
	}
	bounds := image.Rect(0, 0, baseConfig.Width, baseConfig.Height)
	annotated := image.NewRGBA(bounds)
	draw.Draw(annotated, bounds, base, base.Bounds().Min, draw.Src)
	draw.Draw(annotated, bounds, overlay, overlay.Bounds().Min, draw.Over)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, annotated); err != nil {
		return nil, fmt.Errorf("encode markup image: %w", err)
	}
	if encoded.Len() > maxImageReferenceBytes {
		return nil, errors.New("annotated image exceeds 50MB limit")
	}
	result := append([][]byte(nil), inputs...)
	result[0] = encoded.Bytes()
	if err := validateImageInputSet(result); err != nil {
		return nil, err
	}
	return result, nil
}
