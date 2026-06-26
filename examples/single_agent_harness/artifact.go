package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Artifact holds a versioned file created by the agent.
type Artifact struct {
	ID        string
	SessionID string
	Filename  string
	Content   []byte
	MimeType  string
	Version   int
	IsText    bool
	CreatedAt time.Time
}

// TextContent returns the content as a string (valid only when IsText is true).
func (a *Artifact) TextContent() string { return string(a.Content) }

// LineCount returns the number of lines in the text content.
func (a *Artifact) LineCount() int {
	if !a.IsText {
		return 0
	}
	return strings.Count(string(a.Content), "\n") + 1
}

// ── Registry ──────────────────────────────────────────────────────────────────

var (
	artifactsMu sync.Mutex
	// sessions: sessionID → filename → *Artifact (latest version)
	artifacts = map[string]map[string]*Artifact{}
	// byID: artifactID → *Artifact (all versions)
	artifactsByID = map[string]*Artifact{}
	artCounter    int
)

func putArtifact(sessionID, filename string, content []byte, mime string) *Artifact {
	isText := isTextMime(mime)

	artifactsMu.Lock()
	defer artifactsMu.Unlock()

	if _, ok := artifacts[sessionID]; !ok {
		artifacts[sessionID] = map[string]*Artifact{}
	}

	version := 1
	if prev, ok := artifacts[sessionID][filename]; ok {
		version = prev.Version + 1
	}

	artCounter++
	id := fmt.Sprintf("art-%d-%d", time.Now().UnixNano(), artCounter)

	art := &Artifact{
		ID:        id,
		SessionID: sessionID,
		Filename:  filename,
		Content:   content,
		MimeType:  mime,
		Version:   version,
		IsText:    isText,
		CreatedAt: time.Now(),
	}
	artifacts[sessionID][filename] = art
	artifactsByID[id] = art
	return art
}

func getArtifact(sessionID, filename string) *Artifact {
	artifactsMu.Lock()
	defer artifactsMu.Unlock()
	if sess, ok := artifacts[sessionID]; ok {
		return sess[filename]
	}
	return nil
}

func listArtifacts(sessionID string) []*Artifact {
	artifactsMu.Lock()
	defer artifactsMu.Unlock()
	sess := artifacts[sessionID]
	out := make([]*Artifact, 0, len(sess))
	for _, a := range sess {
		out = append(out, a)
	}
	return out
}

func getArtifactByID(id string) *Artifact {
	artifactsMu.Lock()
	defer artifactsMu.Unlock()
	return artifactsByID[id]
}

// ── MIME helpers ──────────────────────────────────────────────────────────────

var textMimes = map[string]bool{
	"text/plain": true, "text/html": true, "text/css": true,
	"text/csv": true, "text/markdown": true, "text/x-python": true,
	"text/x-sh": true, "text/xml": true, "text/yaml": true,
	"application/json": true, "image/svg+xml": true,
}

func isTextMime(mime string) bool {
	return textMimes[mime] || strings.HasPrefix(mime, "text/")
}

func guessMime(filename string) string {
	dot := strings.LastIndex(filename, ".")
	if dot < 0 {
		return "text/plain"
	}
	ext := strings.ToLower(filename[dot+1:])
	m := map[string]string{
		"svg": "image/svg+xml", "html": "text/html",
		"csv": "text/csv", "json": "application/json",
		"txt": "text/plain", "py": "text/x-python",
		"go": "text/x-go", "js": "text/javascript", "ts": "text/typescript",
		"md": "text/markdown", "sh": "text/x-sh",
		"xml": "text/xml", "yaml": "text/yaml", "yml": "text/yaml",
		"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg",
		"gif": "image/gif", "webp": "image/webp", "pdf": "application/pdf",
		"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"zip":  "application/zip",
	}
	if mime, ok := m[ext]; ok {
		return mime
	}
	return "text/plain"
}
