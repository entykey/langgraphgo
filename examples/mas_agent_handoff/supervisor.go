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
	RouteJSON(ctx context.Context, systemPrompt, userPrompt string) (raw string, promptTok, completionTok int, err error)
	StreamReply(ctx context.Context, systemPrompt string, msgs []Message, onToken func(string)) (string, time.Time, error)
}

// supervisorRoutingPrompt classifies each turn into exactly one of two buckets.
// web_search is no longer a route — the supervisor handles it as a tool in self-reply.
const supervisorRoutingPrompt = `You are a routing supervisor. Classify the user's latest request into exactly one option:

  json_agent  — any query about JSONPlaceholder data: users, posts, todos, comments
  self        — everything else: greetings, small talk, factual questions, web searches,
                visual follow-up questions, health/science/general knowledge

RULE: When in doubt, always choose self.
Reply ONLY JSON: {"next":"json_agent"|"self","reasoning":"one short sentence"}`

// supervisorChatPrompt is used when the DeepSeek supervisor answers directly.
// It has access to the web_search tool and should use it for live information.
const supervisorChatPrompt = `Bạn là trợ lý thông minh, thân thiện. Trả lời bằng tiếng Việt.
Nhớ lịch sử hội thoại. Không bịa thông tin.
Khi cần thông tin thực tế mới nhất hoặc không chắc chắn, hãy gọi tool web_search.
Khi có kết quả web_search, tổng hợp câu trả lời rõ ràng và đính kèm nguồn tham khảo.`

// supervisorChatPromptSimple is used for the Gemini supervisor fallback (no tool calling).
const supervisorChatPromptSimple = `Bạn là trợ lý thân thiện. Trả lời ngắn gọn, tự nhiên bằng tiếng Việt.
Nhớ lịch sử hội thoại. Không bịa dữ liệu.`

type routingDecision struct {
	Reasoning string `json:"reasoning"`
	Next      string `json:"next"`
}

// supervisorNode routes the request or self-replies.
// When backend is *DSClient: self-reply uses a DeepSeek ReAct loop with web_search as a tool,
// so web results are fetched and synthesised by the supervisor itself.
// When backend is *GeminiClient: falls back to simple StreamReply (no tool calling).
func supervisorNode(backend SupervisorBackend, gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	ds, isDS := backend.(*DSClient)

	return func(ctx context.Context, state AgentState) (AgentState, error) {
		state.Step++
		emit(state.EventCh, "node_start", map[string]string{"node": "supervisor"})

		if state.Step > 8 {
			state.Next = "FINISH"
			return state, nil
		}

		// Image present → skip LLM routing, go straight to vision_agent.
		if state.ImageB64 != "" {
			emit(state.EventCh, "routing", map[string]string{
				"decision":  "vision_agent",
				"reasoning": fmt.Sprintf("image detected (%s) → Gemini VLM", state.ImageMime),
			})
			state.Next = "vision_agent"
			return state, nil
		}

		// Build routing prompt from conversation history.
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

		globalLF.GenerationEnd(routeGenID, state.TraceID, map[string]any{"routing_json": raw},
			promptTok, completionTok, time.Time{})

		var dec routingDecision
		if json.Unmarshal([]byte(raw), &dec) != nil {
			dec.Next = "self"
			dec.Reasoning = "parse error"
		}

		next := strings.ToLower(strings.TrimSpace(dec.Next))
		if next != "json_agent" {
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

		// ── Self-reply ────────────────────────────────────────────────────────────
		emit(state.EventCh, "node_start", map[string]string{"node": "supervisor_reply"})

		if isDS {
			return supervisorReplyWithTools(ctx, state, ds, gemini, spanID)
		}
		return supervisorReplySimple(ctx, state, backend, spanID)
	}
}

// supervisorReplyWithTools runs a DeepSeek ReAct loop with web_search available as a tool.
// The supervisor decides on its own whether to call web_search or answer directly.
func supervisorReplyWithTools(ctx context.Context, state AgentState, ds *DSClient, gemini *GeminiClient, spanID string) (AgentState, error) {
	wsTool := makeWebSearchTool(ctx, gemini, state.EventCh)
	wsAPITools := buildAPITools([]ToolDef{wsTool})

	dsMsgs := []dsChatMsg{{Role: "system", Content: strPtr(supervisorChatPrompt)}}
	for _, m := range state.Messages {
		role := m.Role
		if role == "model" {
			role = "assistant"
		}
		dsMsgs = append(dsMsgs, dsChatMsg{Role: role, Content: strPtr(m.Content)})
	}

	fullText := ""
	var replyFirstDelta time.Time

	for round := 0; round < 4; round++ {
		replyID := lfUUID()
		globalLF.GenerationStart(replyID, state.TraceID, spanID,
			fmt.Sprintf("supervisor-reply-r%d", round+1), supervisorModel,
			lfDSMsgs(dsMsgs, 8))

		resp, promptTok, completionTok, firstDelta, err := ds.StreamChatWithTools(
			ctx, dsMsgs, wsAPITools, nil,
			func(tok string) {
				emit(state.EventCh, "token", map[string]string{"text": tok})
			},
		)
		if err != nil {
			globalLF.GenerationEnd(replyID, state.TraceID, map[string]any{"error": err.Error()}, promptTok, completionTok, time.Time{})
			return state, fmt.Errorf("supervisor reply: %w", err)
		}
		if replyFirstDelta.IsZero() {
			replyFirstDelta = firstDelta
		}

		// Final answer round — no tool calls.
		if len(resp.ToolCalls) == 0 {
			if resp.Content != nil {
				fullText = *resp.Content
			}
			globalLF.GenerationEnd(replyID, state.TraceID,
				map[string]any{"text": truncate(fullText, 300)},
				promptTok, completionTok, replyFirstDelta)
			break
		}

		// Tool call round.
		globalLF.GenerationEnd(replyID, state.TraceID,
			map[string]any{"tool_calls": len(resp.ToolCalls)},
			promptTok, completionTok, firstDelta)
		dsMsgs = append(dsMsgs, *resp)

		for _, tc := range resp.ToolCalls {
			var args map[string]any
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
				args = map[string]any{}
			}
			toolEvt := map[string]string{"name": tc.Function.Name}
			if q, _ := args["query"].(string); q != "" {
				toolEvt["query"] = q
			}
			emit(state.EventCh, "tool_call", toolEvt)
			fmt.Printf("[supervisor] tool: %s(%s)\n", tc.Function.Name, tc.Function.Arguments)

			toolSpanID := lfUUID()
			globalLF.SpanStart(toolSpanID, state.TraceID, spanID, tc.Function.Name,
				map[string]any{"args": tc.Function.Arguments})
			result := wsTool.Fn(args)
			globalLF.SpanEnd(toolSpanID, state.TraceID,
				map[string]any{"result": truncate(result, 300)})

			dsMsgs = append(dsMsgs, dsChatMsg{
				Role:       "tool",
				Content:    strPtr(result),
				ToolCallID: tc.ID,
			})
		}
	}

	state.Messages = append(state.Messages, Message{Role: "model", Content: fullText, Name: "supervisor"})
	state.Next = "FINISH"
	return state, nil
}

// supervisorReplySimple is used when the Gemini backend is active (no tool calling).
func supervisorReplySimple(ctx context.Context, state AgentState, backend SupervisorBackend, spanID string) (AgentState, error) {
	replyID := lfUUID()
	replyInput := append([]map[string]any{{"role": "system", "content": supervisorChatPromptSimple}},
		lfMsgs(state.Messages, 8)...)
	globalLF.GenerationStart(replyID, state.TraceID, spanID, "supervisor-reply", supervisorModel, replyInput)

	text, firstDelta, err := backend.StreamReply(ctx, supervisorChatPromptSimple, state.Messages, func(tok string) {
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
