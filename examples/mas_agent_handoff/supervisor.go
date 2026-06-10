package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const supervisorRoutingPrompt = `You are a routing supervisor. Classify the user's latest request into exactly one option:

  json_agent  — any query about JSONPlaceholder data: users, posts, todos, comments
  web_search  — needs live web info: news, prices, recent events, real-world facts
  self        — greeting, small talk, or conversational question with NO data involved

RULE: When in doubt between json_agent and self, always choose json_agent.
Reply ONLY JSON: {"reasoning":"...","next":"json_agent"|"web_search"|"self"}`

const supervisorChatPrompt = `Bạn là trợ lý thân thiện. Trả lời ngắn gọn, tự nhiên bằng tiếng Việt.
Nhớ lịch sử hội thoại. Không bịa dữ liệu — nếu cần dữ liệu hãy nói cần gọi tool.`

type routingDecision struct {
	Reasoning string `json:"reasoning"`
	Next      string `json:"next"`
}

// supervisorNode routes the request or self-replies.
// It emits: node_start, routing (if routing), token/done (if self-reply).
func supervisorNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		state.Step++
		emit(state.EventCh, "node_start", map[string]string{"node": "supervisor"})

		if state.Step > 8 {
			state.Next = "FINISH"
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

		t0 := time.Now()
		raw, err := gemini.RouteJSON(ctx, supervisorRoutingPrompt, userPrompt)
		if err != nil {
			// fallback to self on routing error
			fmt.Printf("[supervisor] routing error: %v — fallback to self\n", err)
			state.Next = "self"
			emit(state.EventCh, "routing", map[string]string{"decision": "self", "reasoning": "routing error"})
			return state, nil
		}
		fmt.Printf("[supervisor] routing in %.2fs: %s\n", time.Since(t0).Seconds(), raw)

		var dec routingDecision
		if json.Unmarshal([]byte(raw), &dec) != nil {
			dec.Next = "self"
			dec.Reasoning = "parse error"
		}

		next := strings.ToLower(strings.TrimSpace(dec.Next))
		if next != "json_agent" && next != "web_search" && next != "self" {
			next = "self"
		}
		state.Next = next
		emit(state.EventCh, "routing", map[string]string{"decision": next, "reasoning": dec.Reasoning})

		if next != "self" {
			return state, nil
		}

		// Self-reply: stream tokens
		emit(state.EventCh, "node_start", map[string]string{"node": "supervisor_reply"})
		tokCh := make(chan string, 256)
		errc := make(chan error, 1)
		go func() {
			errc <- gemini.StreamChat(ctx, supervisorChatPrompt, state.Messages, tokCh)
			close(tokCh)
		}()

		var sb strings.Builder
		for tok := range tokCh {
			sb.WriteString(tok)
			emit(state.EventCh, "token", map[string]string{"text": tok})
		}
		if err := <-errc; err != nil {
			return state, fmt.Errorf("supervisor self-reply: %w", err)
		}

		state.Messages = append(state.Messages, Message{Role: "model", Content: sb.String(), Name: "supervisor"})
		state.Next = "FINISH"
		return state, nil
	}
}
