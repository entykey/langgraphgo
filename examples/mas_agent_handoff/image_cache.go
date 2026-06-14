package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// imageEntry holds image metadata stored in imageCache.
// MinIO mode: B64 is empty — bytes live in object storage.
// Memory mode: B64 holds the full base64-encoded image.
type imageEntry struct {
	Mime string
	B64  string
}

var (
	imageCache sync.Map // id → imageEntry
	b64Cache   sync.Map // id → base64 string (populated on first LLM fetch, avoids repeat MinIO fetch+encode)
	hashIndex  sync.Map // sha256hex(stored bytes) → image_id (in-process dedup)
)

// compressThreshold: files above this size are resized + re-encoded as JPEG at upload time.
// Reduces MinIO storage and eliminates on-the-fly compression per chat turn.
const compressThreshold = 1 << 20 // 1 MB

// uploadHandler accepts multipart/form-data with a single "image" field.
// Pipeline: read → compress (if decodable + large) → dedup check → store → respond.
// EXIF is automatically stripped during JPEG re-encode (Go encoder writes pixel data only).
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

	raw, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	mime := hdr.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}

	// Compress at upload: resize to max 1536 px + JPEG 85% if decodable and > 1 MB.
	data, finalMime, compressMs := compressForStorage(raw, mime)

	// Content hash dedup: same compressed bytes → reuse existing ID, skip MinIO write.
	hash := sha256Hex(data)
	if existingID, hit := hashIndex.Load(hash); hit {
		id := existingID.(string)
		fmt.Printf("[upload:dedup] %s (%s → %s) → %s (duplicate, skipped store)\n",
			hdr.Filename, fmtSize(len(raw)), fmtSize(len(data)), id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"image_id": id,
			"mime":     finalMime,
			"size_kb":  len(data) / 1024,
			"backend":  "dedup",
		})
		return
	}

	id := lfUUID()
	t0 := time.Now()

	backend := "memory"
	if minioEnabled() {
		if err := minioPut(id, finalMime, data); err != nil {
			http.Error(w, "object storage error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		imageCache.Store(id, imageEntry{Mime: finalMime})
		backend = "minio"
	} else {
		imageCache.Store(id, imageEntry{
			Mime: finalMime,
			B64:  base64.StdEncoding.EncodeToString(data),
		})
	}

	hashIndex.Store(hash, id)
	storeMs := time.Since(t0).Milliseconds()

	// Build log line: show compress time only when compression actually ran.
	var sb strings.Builder
	fmt.Fprintf(&sb, "[upload:%s] %s (%s", backend, hdr.Filename, fmtSize(len(raw)))
	if len(data) != len(raw) {
		fmt.Fprintf(&sb, " → %s, compress:%dms", fmtSize(len(data)), compressMs)
	} else {
		fmt.Fprintf(&sb, ", %s", finalMime)
	}
	fmt.Fprintf(&sb, ") store:%dms → %s", storeMs, id)
	fmt.Println(sb.String())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"image_id": id,
		"mime":     finalMime,
		"size_kb":  len(data) / 1024,
		"backend":  backend,
	})
}

// compressForStorage compresses image bytes if the format is decodable and size > threshold.
// Returns (finalData, finalMime, compressMs). compressMs=0 means no compression was applied.
func compressForStorage(data []byte, mime string) ([]byte, string, int64) {
	if len(data) <= compressThreshold {
		return data, mime, 0
	}
	t0 := time.Now()
	compressed, err := resizeForVLM(data)
	if err != nil || len(compressed) >= len(data) {
		return data, mime, 0 // unsupported format (AVIF etc.) or no saving — pass through
	}
	return compressed, "image/jpeg", time.Since(t0).Milliseconds()
}

// sha256Hex returns the hex-encoded SHA-256 digest of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
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
// Images are already compressed at upload time. Results are cached in b64Cache so
// subsequent turns reusing the same image_id skip MinIO fetch and re-encoding entirely.
func getImageForLLM(id string) (b64, mime string, ok bool) {
	entry, found := lookupImage(id)
	if !found {
		return "", "", false
	}

	// b64Cache hit — zero MinIO round-trip, zero re-encode
	if cached, hit := b64Cache.Load(id); hit {
		return cached.(string), entry.Mime, true
	}

	var data []byte
	if entry.B64 != "" {
		// memory mode
		data, _ = base64.StdEncoding.DecodeString(entry.B64)
		mime = entry.Mime
	} else {
		// MinIO mode: fetch once, then cache
		var fetchedMime string
		var fetchErr error
		data, fetchedMime, fetchErr = minioGetBytes(id)
		if fetchErr != nil {
			fmt.Printf("[image] MinIO fetch error for %s: %v\n", id, fetchErr)
			return "", "", false
		}
		mime = fetchedMime
		if mime == "" {
			mime = entry.Mime
		}
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	b64Cache.Store(id, encoded)
	return encoded, mime, true
}

const vlmMaxPx = 1536 // Gemini tile boundary (768×2)

// resizeForVLM decodes image bytes, scales the longest side to vlmMaxPx if needed,
// and re-encodes as JPEG 85%. Returns error for unsupported formats (AVIF, WebP, etc.).
func resizeForVLM(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	var dst image.Image
	if w > vlmMaxPx || h > vlmMaxPx {
		scale := float64(vlmMaxPx) / float64(max(w, h))
		dw := int(float64(w) * scale)
		dh := int(float64(h) * scale)
		rgba := image.NewRGBA(image.Rect(0, 0, dw, dh))
		draw.BiLinear.Scale(rgba, rgba.Bounds(), src, b, draw.Src, nil)
		dst = rgba
	} else {
		dst = src
	}

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

// imageHandler serves a stored image by ID.
// MinIO mode: 302 redirect to a 1-hour presigned URL — browser fetches directly from MinIO,
// Go is not in the data path at all.
// Memory fallback: serves bytes from imageCache.
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

	if minioEnabled() {
		if u, err := minioPresignURL(id, time.Hour); err == nil {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
		// presign failed — stream as fallback
		w.Header().Set("Cache-Control", "private, max-age=3600")
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
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Type", entry.Mime)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data) //nolint:errcheck
}
