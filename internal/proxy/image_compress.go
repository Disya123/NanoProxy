package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// CompressImages recursively scans a JSON payload for base64 image data URIs
// (data:image/png;base64,...), decodes them, compresses them to JPEG with the
// specified quality, and returns the modified JSON.
func CompressImages(body []byte, quality int) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		// Not a JSON object or array, just return as-is
		return body, nil
	}

	changed := compressRecursive(m, quality)
	if !changed {
		return body, nil
	}
	return json.Marshal(m)
}

func compressRecursive(v any, quality int) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && strings.HasPrefix(s, "data:image/") {
				if newS, ok := compressBase64Image(s, quality); ok {
					x[k] = newS
					changed = true
				}
			} else {
				if compressRecursive(val, quality) {
					changed = true
				}
			}
		}
	case []any:
		for i, val := range x {
			if s, ok := val.(string); ok && strings.HasPrefix(s, "data:image/") {
				if newS, ok := compressBase64Image(s, quality); ok {
					x[i] = newS
					changed = true
				}
			} else {
				if compressRecursive(val, quality) {
					changed = true
				}
			}
		}
	}
	return changed
}

func compressBase64Image(dataURI string, quality int) (string, bool) {
	// Format: data:image/png;base64,iVBORw0KGgo...
	parts := strings.SplitN(dataURI, ",", 2)
	if len(parts) != 2 {
		return "", false
	}
	header := parts[0]
	if !strings.HasSuffix(header, ";base64") {
		return "", false
	}

	b, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}

	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return "", false
	}

	// Calculate new dimensions (max 2048 on any side)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	const maxDim = 2048
	if width > maxDim || height > maxDim {
		if width > height {
			height = (height * maxDim) / width
			width = maxDim
		} else {
			width = (width * maxDim) / height
			height = maxDim
		}
	}

	newBounds := image.Rect(0, 0, width, height)
	opaque := image.NewRGBA(newBounds)

	// Fill with white background
	draw.Draw(opaque, opaque.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)

	// Scale and draw the original image onto the opaque canvas
	xdraw.ApproxBiLinear.Scale(opaque, opaque.Bounds(), img, bounds, draw.Over, nil)

	var buf bytes.Buffer
	err = jpeg.Encode(&buf, opaque, &jpeg.Options{Quality: quality})
	if err != nil {
		return "", false
	}

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	return "data:image/jpeg;base64," + encoded, true
}
