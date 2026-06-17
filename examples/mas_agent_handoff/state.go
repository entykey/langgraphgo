package main

import (
	"encoding/json"
	"fmt"
)

// Message is a single conversation turn.
type Message struct {
	Role    string `json:"role"` // "user" | "model"
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// FileEntry holds metadata for a file in the conversation.
// Content is NOT stored here — only in ArtifactStore after the file is read by a tool.
type FileEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`    // original filename, e.g. "invoice.xlsx"
	Mime    string `json:"mime"`    // full MIME type
	SizeKB  int    `json:"size_kb"`
	Status  string `json:"status"`  // "available" | "read" | "error"
	Summary string `json:"summary,omitempty"` // 1-line summary after first read
}

// AgentState flows through the LangGraphGo graph.
// EventCh is a per-request write-only channel; nodes emit SSE events through it.
type AgentState struct {
	Messages      []Message
	FileRegistry  []FileEntry       // file metadata visible to supervisor at all times
	ArtifactStore map[string]string // file_id → extracted content (populated lazily on read)
	Next          string
	EventCh       chan<- SSEEvent
	Step          int
	TraceID       string // Langfuse trace ID for this request turn
	SessionID     string // from HTTP request, groups turns into a session
	ImageB64      string // base64-encoded image for the current turn (empty = text-only)
	ImageMime     string // MIME type e.g. "image/jpeg" (empty when no image)
}

// SSEEvent is one server-sent event emitted by a graph node.
type SSEEvent struct {
	Type string
	Data any
}

// Encode formats the event in SSE wire format.
func (e SSEEvent) Encode() string {
	b, _ := json.Marshal(e.Data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", e.Type, string(b))
}

// emit is a helper used by nodes to send typed SSE events without nil-checking.
func emit(ch chan<- SSEEvent, typ string, data any) {
	if ch != nil {
		ch <- SSEEvent{Type: typ, Data: data}
	}
}
