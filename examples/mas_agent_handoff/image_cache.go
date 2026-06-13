package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

	fmt.Printf("[upload:%s] %s (%s, %dKB) → %s\n", backend, hdr.Filename, mime, len(data)/1024, id)

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
// In MinIO mode it fetches bytes from object storage and encodes them on the fly.
func getImageForLLM(id string) (b64, mime string, ok bool) {
	entry, found := lookupImage(id)
	if !found {
		return "", "", false
	}
	if entry.B64 != "" {
		// memory mode: already encoded
		return entry.B64, entry.Mime, true
	}
	// MinIO mode: fetch bytes, encode
	data, fetchedMime, err := minioGetBytes(id)
	if err != nil {
		fmt.Printf("[image] MinIO fetch error for %s: %v\n", id, err)
		return "", "", false
	}
	if fetchedMime != "" {
		mime = fetchedMime
	} else {
		mime = entry.Mime
	}
	return base64.StdEncoding.EncodeToString(data), mime, true
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
