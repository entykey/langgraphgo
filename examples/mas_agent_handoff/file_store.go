package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// docStore is the in-memory fallback for document bytes (non-image files).
// MinIO mode uses the "docs/" prefix in the same bucket as images.
var docStore sync.Map // file_id → []byte

// storeDocument saves raw document bytes to MinIO (prefix "docs/") or in-memory.
func storeDocument(id, mime string, data []byte) error {
	if minioEnabled() {
		return minioPut("docs/"+id, mime, data)
	}
	docStore.Store(id, append([]byte(nil), data...))
	return nil
}

// fetchDocument retrieves raw document bytes by ID.
func fetchDocument(id string) ([]byte, error) {
	if minioEnabled() {
		data, _, err := minioGetBytes("docs/" + id)
		return data, err
	}
	v, ok := docStore.Load(id)
	if !ok {
		return nil, fmt.Errorf("document %s not found (expired or server restarted)", id)
	}
	return v.([]byte), nil
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
