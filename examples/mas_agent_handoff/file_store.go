package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// docEntry holds raw bytes and MIME type for a stored document.
type docEntry struct {
	Data []byte
	Mime string
}

// docStore is the in-memory fallback for document bytes (non-image files).
// MinIO mode uses the "docs/" prefix in the same bucket as images.
var docStore sync.Map // file_id → docEntry

// storeDocument saves raw document bytes to MinIO (prefix "docs/") or in-memory.
func storeDocument(id, mime string, data []byte) error {
	if minioEnabled() {
		return minioPut("docs/"+id, mime, data)
	}
	docStore.Store(id, docEntry{Data: append([]byte(nil), data...), Mime: mime})
	return nil
}

// fetchDocument retrieves raw document bytes and MIME type by ID.
// MIME is returned so callers don't need to re-detect from magic bytes.
func fetchDocument(id string) (data []byte, mime string, err error) {
	if minioEnabled() {
		return minioGetBytes("docs/" + id)
	}
	v, ok := docStore.Load(id)
	if !ok {
		return nil, "", fmt.Errorf("document %s not found (expired or server restarted)", id)
	}
	e := v.(docEntry)
	return e.Data, e.Mime, nil
}

// detectMimeByExt maps a filename extension to a MIME type.
// Used when the browser does not set Content-Type on the upload.
func detectMimeByExt(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".xlsx", ".xlsm":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".csv":
		return "text/csv"
	case ".txt":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
}
