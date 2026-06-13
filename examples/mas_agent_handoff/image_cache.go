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

// imageEntry holds base64-encoded image data ready to send to Gemini.
// Stored by UUID returned to the client at upload time.
type imageEntry struct {
	B64  string // base64-encoded, no data-URL prefix
	Mime string // e.g. "image/jpeg"
}

// imageCache is a single-use in-memory store: entries are deleted on first retrieval.
// No TTL needed for a lab — abandoned uploads are small and disappear on server restart.
var imageCache sync.Map

// uploadHandler accepts multipart/form-data with a single "image" field,
// stores it in imageCache, and returns { image_id, mime, size_kb }.
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
	imageCache.Store(id, imageEntry{
		B64:  base64.StdEncoding.EncodeToString(data),
		Mime: mime,
	})

	fmt.Printf("[upload] stored %s (%s, %dKB) → %s\n", hdr.Filename, mime, len(data)/1024, id)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"image_id": id,
		"mime":     mime,
		"size_kb":  len(data) / 1024,
	})
}

// lookupAndConsume retrieves an image entry by ID and removes it (single-use).
func lookupAndConsume(id string) (imageEntry, bool) {
	v, ok := imageCache.LoadAndDelete(id)
	if !ok {
		return imageEntry{}, false
	}
	return v.(imageEntry), true
}

// lookupImage retrieves an image entry by ID without removing it.
func lookupImage(id string) (imageEntry, bool) {
	v, ok := imageCache.Load(id)
	if !ok {
		return imageEntry{}, false
	}
	return v.(imageEntry), true
}

// imageHandler serves a stored image by ID (read-only, does not consume the entry).
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
	entry, ok := lookupImage(id)
	if !ok {
		http.Error(w, "not found — image may have expired or server restarted", http.StatusNotFound)
		return
	}
	data, err := base64.StdEncoding.DecodeString(entry.B64)
	if err != nil {
		http.Error(w, "decode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", entry.Mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data) //nolint:errcheck
}
