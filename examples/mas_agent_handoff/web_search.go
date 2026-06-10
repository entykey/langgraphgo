package main

import (
	"context"
	"fmt"
	"strings"
)

// webSearchNode calls Gemini with Google Search grounding (non-streaming to preserve citations).
func webSearchNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		emit(state.EventCh, "node_start", map[string]string{"node": "web_search"})
		fmt.Println("[web_search] starting")

		text, citations, err := gemini.WebSearch(ctx, state.Messages)
		if err != nil {
			return state, fmt.Errorf("web_search: %w", err)
		}

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
