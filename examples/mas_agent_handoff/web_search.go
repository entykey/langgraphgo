package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// webSearchNode calls Gemini with Google Search grounding (non-streaming to preserve citations).
func webSearchNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		emit(state.EventCh, "node_start", map[string]string{"node": "web_search"})
		fmt.Println("[web_search] starting")

		lastUser := ""
		for i := len(state.Messages) - 1; i >= 0; i-- {
			if state.Messages[i].Role == "user" {
				lastUser = state.Messages[i].Content
				break
			}
		}
		genID := lfUUID()
		globalLF.GenerationStart(genID, state.TraceID, "", "web_search", gemModel,
			[]map[string]any{
				{"role": "system", "content": webSearchSystemPrompt},
				{"role": "user", "content": lastUser},
			})

		text, citations, promptTok, completionTok, err := gemini.WebSearch(ctx, state.Messages)
		if err != nil {
			globalLF.GenerationEnd(genID, state.TraceID, map[string]any{"error": err.Error()}, 0, 0, time.Time{})
			return state, fmt.Errorf("web_search: %w", err)
		}
		// WebSearch is non-streaming (returns full text at once) — no per-token TTFT.
		globalLF.GenerationEnd(genID, state.TraceID, map[string]any{
			"text_preview": truncate(text, 300),
			"citations":    len(citations),
		}, promptTok, completionTok, time.Time{})

		if len(citations) > 0 {
			emit(state.EventCh, "citations", map[string]any{"count": len(citations)})
			var sb strings.Builder
			sb.WriteString(text)
			sb.WriteString("\n\n---\n**Nguồn:**\n")
			for i, c := range citations {
				fmt.Fprintf(&sb, "%d. [%s](%s)\n", i+1, c.Title, c.URL)
			}
			text = sb.String()
		}

		if strings.TrimSpace(text) == "" {
			text = "Không tìm thấy kết quả từ Google Search."
		}

		emit(state.EventCh, "token_batch", map[string]string{"text": text})

		state.Messages = append(state.Messages, Message{
			Role:    "model",
			Content: text,
			Name:    "web_search",
		})
		return state, nil
	}
}
