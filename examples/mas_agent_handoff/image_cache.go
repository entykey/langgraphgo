package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

// imageEntry holds image metadata.
// When MinIO is enabled, B64 is empty — bytes live in object storage.
// When using in-memory fallback, B64 holds the base64-encoded image.
type imageEntry struct {
	B64  string // non-empty only in memory-only mode
	Mime string // e.g. "image/jpeg"
}

// imageCache stores either full base64 entries (memory mode) or mime-only metadata (MinIO mode).
// Entries persist for the lifetime of the server process.
var imageCache sync.Map

// uploadHandler accepts multipart/form-data with a single "image" field.
// With MinIO: stores bytes in object storage, keeps only mime in imageCache.
// Without MinIO: base64-encodes and stores entirely in imageCache.
// Returns { image_id, mime, size_kb, backend }.
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		http.Error(w, "too large or malformed (max 20 MB)", http.StatusRequestEntityTooLarge)
		return
	}

	file, hdr, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "missing 'image' field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	mime := hdr.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}
	id := lfUUID()
	t0 := time.Now()

	backend := "memory"
	if minioEnabled() {
		if err := minioPut(id, mime, data); err != nil {
			http.Error(w, "object storage error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		imageCache.Store(id, imageEntry{Mime: mime}) // no B64 — bytes are in MinIO
		backend = "minio"
	} else {
		imageCache.Store(id, imageEntry{
			B64:  base64.StdEncoding.EncodeToString(data),
			Mime: mime,
		})
	}

	fmt.Printf("[upload:%s] %s (%s, %s) → %s  [%s]\n",
		backend, hdr.Filename, mime, fmtSize(len(data)), id,
		fmtDuration(time.Since(t0)))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"image_id": id,
		"mime":     mime,
		"size_kb":  len(data) / 1024,
		"backend":  backend,
	})
}

// lookupImage retrieves metadata by ID without removing it from cache.
func lookupImage(id string) (imageEntry, bool) {
	v, ok := imageCache.Load(id)
	if !ok {
		return imageEntry{}, false
	}
	return v.(imageEntry), true
}

// getImageForLLM returns a base64-encoded string ready to embed in an LLM request.
// Images larger than 1.5 MB are resized to max 1536 px and re-encoded as JPEG 85%
// before being sent to the VLM — reducing HTTP payload without affecting token count
// (Gemini tokenises by pixel dimensions, not byte size).
// Formats that can't be decoded by the standard library (AVIF, WebP) pass through unchanged.
func getImageForLLM(id string) (b64, mime string, ok bool) {
	entry, found := lookupImage(id)
	if !found {
		return "", "", false
	}

	var data []byte
	if entry.B64 != "" {
		// memory mode: decode from stored base64
		data, _ = base64.StdEncoding.DecodeString(entry.B64)
		mime = entry.Mime
	} else {
		// MinIO mode: fetch bytes
		var fetchedMime string
		var err error
		data, fetchedMime, err = minioGetBytes(id)
		if err != nil {
			fmt.Printf("[image] MinIO fetch error for %s: %v\n", id, err)
			return "", "", false
		}
		if fetchedMime != "" {
			mime = fetchedMime
		} else {
			mime = entry.Mime
		}
	}

	const compressThreshold = 1536 * 1024 // 1.5 MB
	if len(data) > compressThreshold {
		t0 := time.Now()
		if compressed, err := resizeForVLM(data); err == nil {
			fmt.Printf("[image] compressed %s → %s (JPEG) [%s]\n",
				fmtSize(len(data)), fmtSize(len(compressed)), fmtDuration(time.Since(t0)))
			return base64.StdEncoding.EncodeToString(compressed), "image/jpeg", true
		}
		// decode failed (e.g. AVIF) — fall through with original bytes
	}

	return base64.StdEncoding.EncodeToString(data), mime, true
}

const vlmMaxPx = 1536 // Gemini tile boundary (768×2)

// resizeForVLM decodes img bytes, scales down to vlmMaxPx on the longest side
// (only if larger), and re-encodes as JPEG 85%. Returns error if format unsupported.
func resizeForVLM(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= vlmMaxPx && h <= vlmMaxPx {
		// already within limit — just re-encode as JPEG to strip PNG overhead
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// scale to fit within vlmMaxPx × vlmMaxPx, preserve aspect ratio
	scale := float64(vlmMaxPx) / float64(max(w, h))
	dw := int(float64(w) * scale)
	dh := int(float64(h) * scale)

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// imageHandler serves a stored image by ID (read-only).
// In MinIO mode, streams directly from object storage — zero extra copy in Go heap.
// GET /image/{uuid}
func imageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/image/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=3600")

	if minioEnabled() {
		if !minioStream(w, id) {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}

	// Memory fallback
	entry, ok := lookupImage(id)
	if !ok {
		http.Error(w, "not found — image expired or server restarted", http.StatusNotFound)
		return
	}
	data, err := base64.StdEncoding.DecodeString(entry.B64)
	if err != nil {
		http.Error(w, "decode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", entry.Mime)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data) //nolint:errcheck
}
