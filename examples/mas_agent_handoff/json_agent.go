package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const jsonAgentSystemPrompt = `You are a JSON Data Agent. Your ONLY job is to call tools to fetch data from the JSONPlaceholder REST API.

MANDATORY RULES — follow these without exception:
1. CALL A TOOL on your very first response. Never skip this step.
2. JSONPlaceholder data (users, posts, todos, comments) is NOT in your training data. You have zero memory of it.
3. NEVER write user names, IDs, post titles, emails, or any data without first receiving it from a tool call.
4. "I'll fetch it" followed immediately by data = WRONG. Call the tool, receive results, THEN write the answer.
5. After tool results are received, synthesize a clear, helpful response in Vietnamese.

Available tools: list_users · get_user(user_id) · get_posts(user_id) · get_todos(user_id) · get_comments(post_id)`

// jsonAgentNode runs a ReAct tool-calling loop using DeepSeek with streaming.
// Tool-call rounds accumulate both preamble text and tool_call deltas; the
// final answer round streams tokens one by one.
// Emits: node_start, tool_call, tool_retry, tools_done, token.
func jsonAgentNode(ds *DSClient) func(context.Context, AgentState) (AgentState, error) {
	apiTools := buildAPITools(JSONPlaceholderTools)
	tmap := toolsMap(JSONPlaceholderTools)

	return func(ctx context.Context, state AgentState) (AgentState, error) {
		emit(state.EventCh, "node_start", map[string]string{"node": "json_agent"})
		fmt.Println("[json_agent] starting")

		lastUser := ""
		for i := len(state.Messages) - 1; i >= 0; i-- {
			if state.Messages[i].Role == "user" {
				lastUser = state.Messages[i].Content
				break
			}
		}
		spanID := lfUUID()
		globalLF.SpanStart(spanID, state.TraceID, "", "json_agent", map[string]any{"query": lastUser})

		msgs := []dsChatMsg{{Role: "system", Content: strPtr(jsonAgentSystemPrompt)}}
		for _, m := range state.Messages {
			role := m.Role
			if role == "model" {
				role = "assistant"
			}
			msgs = append(msgs, dsChatMsg{Role: role, Content: strPtr(m.Content)})
		}

		var toolsCalled []string
		fullResponse := ""
		didStream := false

		for round := 0; round < 6; round++ {
			t0 := time.Now()
			fmt.Printf("[json_agent] round %d — calling DeepSeek (stream)\n", round+1)

			var toolChoice any
			if round == 0 {
				toolChoice = "required"
			}

			genID := lfUUID()
			globalLF.GenerationStart(genID, state.TraceID, spanID,
				fmt.Sprintf("json-agent-round-%d", round+1), dsModel,
				lfDSMsgs(msgs, 10))

			resp, promptTok, completionTok, firstDelta, err := ds.StreamChatWithTools(ctx, msgs, apiTools, toolChoice, func(tok string) {
				emit(state.EventCh, "token", map[string]string{"text": tok})
				didStream = true
			})
			if err != nil {
				globalLF.GenerationEnd(genID, state.TraceID, map[string]any{"error": err.Error()}, promptTok, completionTok, time.Time{})
				return state, fmt.Errorf("json_agent DeepSeek: %w", err)
			}
			roundOut := map[string]any{"tool_calls": len(resp.ToolCalls)}
			if resp.Content != nil {
				roundOut["content_preview"] = truncate(*resp.Content, 200)
			}
			globalLF.GenerationEnd(genID, state.TraceID, roundOut, promptTok, completionTok, firstDelta)
			emit(state.EventCh, "usage", map[string]any{
				"agent": "json_agent", "prompt_tok": promptTok, "completion_tok": completionTok,
			})
			fmt.Printf("[json_agent] round %d done in %.2fs, tool_calls=%d\n",
				round+1, time.Since(t0).Seconds(), len(resp.ToolCalls))

			msgs = append(msgs, *resp)

			if len(resp.ToolCalls) == 0 {
				if resp.Content != nil {
					fullResponse = *resp.Content
				}
				break
			}

			for _, tc := range resp.ToolCalls {
				name := tc.Function.Name
				emit(state.EventCh, "tool_call", map[string]string{"name": name})
				fmt.Printf("[json_agent] tool: %s(%s)\n", name, tc.Function.Arguments)

				if !contains(toolsCalled, name) {
					toolsCalled = append(toolsCalled, name)
				}

				def, ok := tmap[name]
				var result string
				if !ok {
					result = fmt.Sprintf("Unknown tool: %s", name)
				} else {
					var args map[string]any
					if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
						args = map[string]any{}
					}
					toolSpanID := lfUUID()
					globalLF.SpanStart(toolSpanID, state.TraceID, spanID, name, map[string]any{"args": tc.Function.Arguments})
					result = callWithRetry(def, args, state.EventCh)
					globalLF.SpanEnd(toolSpanID, state.TraceID, map[string]any{"result": truncate(result, 300)})
					emit(state.EventCh, "tool_result", map[string]string{
						"name":   name,
						"result": truncateRune(result, 600),
					})
				}
				fmt.Printf("[json_agent] tool result: %s...\n", truncateRune(result, 80))

				msgs = append(msgs, dsChatMsg{
					Role:       "tool",
					Content:    strPtr(result),
					ToolCallID: tc.ID,
				})
			}
		}

		globalLF.SpanEnd(spanID, state.TraceID, map[string]any{
			"answer":       truncate(fullResponse, 300),
			"tools_called": toolsCalled,
		})

		if len(toolsCalled) > 0 {
			emit(state.EventCh, "tools_done", map[string]any{"tools": toolsCalled})
		}
		// Fall back to token_batch only if streaming produced nothing (edge case).
		if !didStream {
			emit(state.EventCh, "token_batch", map[string]string{"text": fullResponse})
		}

		state.Messages = append(state.Messages, Message{
			Role:    "model",
			Content: fullResponse,
			Name:    "json_agent",
		})
		return state, nil
	}
}

func callWithRetry(def ToolDef, args map[string]any, ch chan<- SSEEvent) string {
	for attempt := 1; attempt <= 3; attempt++ {
		result := func() (r string) {
			defer func() {
				if rec := recover(); rec != nil {
					r = fmt.Sprintf("tool panic: %v", rec)
				}
			}()
			return def.Fn(args)
		}()
		if !strings.HasPrefix(result, "Error:") {
			return result
		}
		if attempt < 3 {
			emit(ch, "tool_retry", map[string]string{"info": fmt.Sprintf("%s (%d/3)", def.Name, attempt)})
		}
	}
	return fmt.Sprintf("[TOOL ERROR] %s failed after 3 attempts", def.Name)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func fmtDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
}

func fmtSize(bytes int) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%d KB", bytes>>10)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// truncateRune truncates at a rune boundary to avoid splitting multi-byte UTF-8 characters.
func truncateRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Walk back from byte n until we're at a rune boundary.
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
