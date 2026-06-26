package main

import (
	"encoding/json"
	"fmt"
)

// Message is a single conversation turn (user or model).
type Message struct {
	Role    string `json:"role"` // "user" | "model"
	Content string `json:"content"`
}

// messageJSON is an alias kept for JSON decoding compatibility.
type messageJSON = Message

// SSEEvent is one server-sent event emitted during an agent turn.
type SSEEvent struct {
	Type string
	Data any
}

// Encode formats the event in SSE wire format.
func (e SSEEvent) Encode() string {
	b, _ := json.Marshal(e.Data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", e.Type, string(b))
}

// emit sends a typed SSE event without nil-checking the channel.
func emit(ch chan<- SSEEvent, typ string, data any) {
	if ch != nil {
		ch <- SSEEvent{Type: typ, Data: data}
	}
}
