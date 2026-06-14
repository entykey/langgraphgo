package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SupervisorBackend is implemented by both GeminiClient and DSClient.
// Swap via SUPERVISOR_BACKEND env var ("gemini" | "deepseek", default: "deepseek").
type SupervisorBackend interface {
	// RouteJSON returns the raw JSON routing decision and prompt/completion token counts.
	RouteJSON(ctx context.Context, systemPrompt, userPrompt string) (raw string, promptTok, completionTok int, err error)
	// StreamReply streams a self-reply and returns (text, firstDelta, error).
	// firstDelta is the time of the first token — used for TTFT in Langfuse.
	StreamReply(ctx context.Context, systemPrompt string, msgs []Message, onToken func(string)) (string, time.Time, error)
}

const supervisorRoutingPrompt = `You are a routing supervisor. Classify the user's latest request into exactly one option:

  json_agent  — any query about JSONPlaceholder data: users, posts, todos, comments
  web_search  — needs live web info: news, prices, recent events, real-world facts
  self        — greeting, small talk, or conversational question with NO data involved

RULE: When in doubt between json_agent and self, always choose json_agent.
Reply ONLY JSON with next first: {"next":"json_agent"|"web_search"|"self","reasoning":"one short sentence"}`

const supervisorChatPrompt = `Bạn là trợ lý thân thiện. Trả lời ngắn gọn, tự nhiên bằng tiếng Việt.
Nhớ lịch sử hội thoại. Không bịa dữ liệu — nếu cần dữ liệu hãy nói cần gọi tool.`

type routingDecision struct {
	Reasoning string `json:"reasoning"`
	Next      string `json:"next"`
}

// supervisorNode routes the request or self-replies.
// It emits: node_start, routing (if routing), token (if self-reply).
func supervisorNode(backend SupervisorBackend) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		state.Step++
		emit(state.EventCh, "node_start", map[string]string{"node": "supervisor"})

		if state.Step > 8 {
			state.Next = "FINISH"
			return state, nil
		}

		// Image present → skip LLM routing, send straight to vision_agent (Gemini VLM).
		if state.ImageB64 != "" {
			emit(state.EventCh, "routing", map[string]string{
				"decision":  "vision_agent",
				"reasoning": fmt.Sprintf("image detected (%s) → Gemini VLM", state.ImageMime),
			})
			state.Next = "vision_agent"
			return state, nil
		}

		// Build routing prompt from conversation history
		lastUser := ""
		for i := len(state.Messages) - 1; i >= 0; i-- {
			if state.Messages[i].Role == "user" {
				lastUser = state.Messages[i].Content
				break
			}
		}
		var lines []string
		for _, m := range state.Messages {
			name := m.Name
			if name == "" {
				name = m.Role
			}
			p := m.Content
			if len(p) > 400 {
				p = p[:400] + "..."
			}
			lines = append(lines, fmt.Sprintf("[%s]: %s", strings.ToUpper(name), p))
		}
		userPrompt := fmt.Sprintf("User request: %s\n\nHistory:\n%s\n\nDecide:", lastUser, strings.Join(lines, "\n"))

		spanID := lfUUID()
		globalLF.SpanStart(spanID, state.TraceID, "", "supervisor", map[string]any{"query": lastUser})

		routeGenID := lfUUID()
		globalLF.GenerationStart(routeGenID, state.TraceID, spanID, "supervisor-routing", supervisorModel,
			[]map[string]any{
				{"role": "system", "content": supervisorRoutingPrompt},
				{"role": "user", "content": userPrompt},
			})

		t0 := time.Now()
		raw, promptTok, completionTok, err := backend.RouteJSON(ctx, supervisorRoutingPrompt, userPrompt)
		if err != nil {
			fmt.Printf("[supervisor] routing error: %v — fallback to self\n", err)
			globalLF.GenerationEnd(routeGenID, state.TraceID, map[string]any{"error": err.Error()}, 0, 0, time.Time{})
			globalLF.SpanEnd(spanID, state.TraceID, map[string]any{"route": "self", "error": err.Error()})
			state.Next = "self"
			emit(state.EventCh, "routing", map[string]string{"decision": "self", "reasoning": "routing error"})
			return state, nil
		}
		elapsed := time.Since(t0)
		fmt.Printf("[supervisor] routing in %.2fs: %s\n", elapsed.Seconds(), raw)

		// Routing is non-streaming — no TTFT to record.
		globalLF.GenerationEnd(routeGenID, state.TraceID, map[string]any{"routing_json": raw},
			promptTok, completionTok, time.Time{})

		var dec routingDecision
		if json.Unmarshal([]byte(raw), &dec) != nil {
			dec.Next = "self"
			dec.Reasoning = "parse error"
		}

		next := strings.ToLower(strings.TrimSpace(dec.Next))
		if next != "json_agent" && next != "web_search" && next != "self" {
			next = "self"
		}
		globalLF.SpanEnd(spanID, state.TraceID, map[string]any{
			"route":      next,
			"reasoning":  dec.Reasoning,
			"latency_ms": elapsed.Milliseconds(),
		})

		state.Next = next
		emit(state.EventCh, "routing", map[string]string{"decision": next, "reasoning": dec.Reasoning})

		if next != "self" {
			return state, nil
		}

		// Self-reply: stream tokens via the configured backend
		emit(state.EventCh, "node_start", map[string]string{"node": "supervisor_reply"})

		replyID := lfUUID()
		replyInput := append([]map[string]any{{"role": "system", "content": supervisorChatPrompt}},
			lfMsgs(state.Messages, 8)...)
		globalLF.GenerationStart(replyID, state.TraceID, spanID, "supervisor-reply", supervisorModel, replyInput)

		text, firstDelta, err := backend.StreamReply(ctx, supervisorChatPrompt, state.Messages, func(tok string) {
			emit(state.EventCh, "token", map[string]string{"text": tok})
		})
		if err != nil {
			globalLF.GenerationEnd(replyID, state.TraceID, map[string]any{"error": err.Error()}, 0, 0, time.Time{})
			return state, fmt.Errorf("supervisor self-reply: %w", err)
		}
		globalLF.GenerationEnd(replyID, state.TraceID, map[string]any{"text": truncate(text, 300)}, 0, 0, firstDelta)

		state.Messages = append(state.Messages, Message{Role: "model", Content: text, Name: "supervisor"})
		state.Next = "FINISH"
		return state, nil
	}
}
