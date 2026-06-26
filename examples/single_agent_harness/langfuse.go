package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// LFClient sends traces to Langfuse via the batch ingestion REST API.
// All calls are non-blocking — events are queued and flushed by a background goroutine.
// This avoids the Python flush()-blocking-SSE-done problem entirely.
type LFClient struct {
	host       string
	authHeader string
	ch         chan lfBatch
	wg         sync.WaitGroup
	disabled   bool
}

type lfBatch struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	Body      map[string]any `json:"body"`
}

var globalLF *LFClient

func initLangfuse() {
	host := os.Getenv("LANGFUSE_HOST")
	pk := os.Getenv("LANGFUSE_PUBLIC_KEY")
	sk := os.Getenv("LANGFUSE_SECRET_KEY")
	if host == "" || pk == "" || sk == "" {
		fmt.Println("[langfuse] disabled — set LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY to enable")
		globalLF = &LFClient{disabled: true}
		return
	}
	auth := base64.StdEncoding.EncodeToString([]byte(pk + ":" + sk))
	globalLF = &LFClient{
		host:       host,
		authHeader: "Basic " + auth,
		ch:         make(chan lfBatch, 2048),
	}
	globalLF.wg.Add(1)
	go globalLF.run()
	fmt.Printf("[langfuse] enabled → %s\n", host)
}

// run batches queued events every 400ms or when 25 events accumulate.
func (c *LFClient) run() {
	defer c.wg.Done()
	batch := make([]lfBatch, 0, 25)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.sendBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev, ok := <-c.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= 25 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (c *LFClient) sendBatch(batch []lfBatch) {
	b, err := json.Marshal(map[string]any{"batch": batch})
	if err != nil {
		return
	}

	// Collect event types for the summary log line.
	types := make([]string, len(batch))
	for i, ev := range batch {
		types[i] = ev.Type
	}
	kb := float64(len(b)) / 1024

	if os.Getenv("LANGFUSE_DEBUG") == "1" {
		sanitized := make([]map[string]any, len(batch))
		for i, ev := range batch {
			sanitized[i] = map[string]any{
				"id":        ev.ID,
				"type":      ev.Type,
				"timestamp": ev.Timestamp,
				"body":      sanitizeAny(ev.Body),
			}
		}
		pretty, _ := json.MarshalIndent(map[string]any{"batch": sanitized}, "", "  ")
		fmt.Printf("[langfuse:debug] payload %.1fKB:\n%s\n", kb, pretty)
	}

	req, err := http.NewRequest("POST", c.host+"/api/public/ingestion", bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.authHeader)
	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(t0)
	if err != nil {
		fmt.Printf("[langfuse] send error after %dms: %v\n", elapsed.Milliseconds(), err)
		return
	}
	resp.Body.Close()
	fmt.Printf("[langfuse] batch %d [%s] → %d (%.1fKB, %dms)\n",
		len(batch), strings.Join(types, ","), resp.StatusCode, kb, elapsed.Milliseconds())
}

// sanitizeAny recursively replaces strings longer than 500 chars with a placeholder.
// Prevents base64 image data from flooding debug logs or Langfuse payloads.
func sanitizeAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = sanitizeAny(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = sanitizeAny(val)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = sanitizeAny(val)
		}
		return out
	case string:
		if len(x) > 500 {
			return fmt.Sprintf("[%d chars]", len(x))
		}
		return x
	default:
		return x
	}
}

func (c *LFClient) enqueue(eventType string, body map[string]any) {
	if c.disabled {
		return
	}
	select {
	case c.ch <- lfBatch{
		ID:        lfUUID(),
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Body:      body,
	}:
	default:
		// channel full — drop silently rather than block hot path
	}
}

// Shutdown flushes remaining events. Call on server exit.
func (c *LFClient) Shutdown() {
	if c.disabled {
		return
	}
	close(c.ch)
	c.wg.Wait()
}

// ── Trace ─────────────────────────────────────────────────────────────────────

func (c *LFClient) TraceCreate(id, sessionID, query string, tags []string) {
	c.enqueue("trace-create", map[string]any{
		"id":        id,
		"name":      "mas-turn",
		"sessionId": sessionID,
		"userId":    "lab-user",
		"tags":      tags,
		"input":     map[string]any{"query": query},
	})
}

func (c *LFClient) TraceUpdate(id, answer string) {
	c.enqueue("trace-update", map[string]any{
		"id":     id,
		"output": map[string]any{"answer": truncate(answer, 500)},
	})
}

// ── Span ──────────────────────────────────────────────────────────────────────

func (c *LFClient) SpanStart(id, traceID, parentID, name string, input any) time.Time {
	t := time.Now().UTC()
	body := map[string]any{
		"id":        id,
		"traceId":   traceID,
		"name":      name,
		"startTime": t.Format(time.RFC3339Nano),
		"input":     input,
	}
	if parentID != "" {
		body["parentObservationId"] = parentID
	}
	c.enqueue("span-create", body)
	return t
}

func (c *LFClient) SpanEnd(id, traceID string, output map[string]any) {
	c.enqueue("span-update", map[string]any{
		"id":      id,
		"traceId": traceID,
		"endTime": time.Now().UTC().Format(time.RFC3339Nano),
		"output":  output,
	})
}

// ── Generation (LLM call) ────────────────────────────────────────────────────

// GenerationStart records an LLM call span. input should be a []map[string]any
// of chat messages (Langfuse renders these as "Chat messages") or any JSON value.
func (c *LFClient) GenerationStart(id, traceID, parentID, name, model string, input any) time.Time {
	t := time.Now().UTC()
	body := map[string]any{
		"id":        id,
		"traceId":   traceID,
		"name":      name,
		"model":     model,
		"startTime": t.Format(time.RFC3339Nano),
		"input":     input,
	}
	if parentID != "" {
		body["parentObservationId"] = parentID
	}
	c.enqueue("generation-create", body)
	return t
}

// GenerationEnd closes a generation span.
// Pass inputTokens=0/outTokens=0 to omit usage.
// Pass a non-zero completionStart to record TTFT via Langfuse's completionStartTime field.
func (c *LFClient) GenerationEnd(id, traceID string, output any, inputTokens, outTokens int, completionStart time.Time) {
	body := map[string]any{
		"id":      id,
		"traceId": traceID,
		"endTime": time.Now().UTC().Format(time.RFC3339Nano),
		"output":  output,
	}
	if inputTokens > 0 || outTokens > 0 {
		body["usage"] = map[string]any{"input": inputTokens, "output": outTokens}
	}
	if !completionStart.IsZero() {
		body["completionStartTime"] = completionStart.UTC().Format(time.RFC3339Nano)
	}
	c.enqueue("generation-update", body)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// lfMsgs formats a Message slice as Langfuse chat-message array (role+content).
// max=0 means include all; positive max takes the last N messages.
func lfMsgs(msgs []Message, max int) []map[string]any {
	start := 0
	if max > 0 && len(msgs) > max {
		start = len(msgs) - max
	}
	out := make([]map[string]any, 0, len(msgs)-start)
	for _, m := range msgs[start:] {
		role := m.Role
		if role == "model" {
			role = "assistant"
		}
		content := m.Content
		if len(content) > 3000 {
			content = content[:3000] + "…"
		}
		out = append(out, map[string]any{"role": role, "content": content})
	}
	return out
}

// lfDSMsgs formats a dsChatMsg slice as Langfuse chat-message array.
func lfDSMsgs(msgs []dsChatMsg, max int) []map[string]any {
	start := 0
	if max > 0 && len(msgs) > max {
		start = len(msgs) - max
	}
	out := make([]map[string]any, 0, len(msgs)-start)
	for _, m := range msgs[start:] {
		content := ""
		if m.Content != nil {
			content = *m.Content
		} else if len(m.ToolCalls) > 0 {
			content = fmt.Sprintf("[%d tool call(s)]", len(m.ToolCalls))
		}
		if len(content) > 3000 {
			content = content[:3000] + "…"
		}
		out = append(out, map[string]any{"role": m.Role, "content": content})
	}
	return out
}

func lfUUID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
