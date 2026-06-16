package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// makeWebSearchTool returns a ToolDef that calls Gemini with Google Search grounding.
// It is registered in the supervisor's self-reply ReAct loop so the supervisor —
// not a standalone node — owns the final answer and can weave citations in naturally.
func makeWebSearchTool(ctx context.Context, gemini *GeminiClient, eventCh chan<- SSEEvent) ToolDef {
	return ToolDef{
		Name:        "web_search",
		Description: "Search the web for current information: news, prices, recent events, medical facts, anything requiring up-to-date data. Returns findings and source citations.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query in Vietnamese or English"
				}
			},
			"required": ["query"]
		}`),
		Fn: func(args map[string]any) string {
			query, _ := args["query"].(string)
			if query == "" {
				return "Error: missing query argument"
			}
			fmt.Printf("[web_search] query: %s\n", query)

			msgs := []Message{{Role: "user", Content: query}}
			text, citations, _, _, err := gemini.StreamWebSearch(ctx, msgs, func(tok string) {
				emit(eventCh, "tool_stream", map[string]string{"name": "web_search", "text": tok})
			})
			if err != nil {
				return "Error searching web: " + err.Error()
			}

			if len(citations) > 0 {
				emit(eventCh, "citations", map[string]any{"count": len(citations)})
			}

			// Return findings + citations as a structured string for DeepSeek to synthesise.
			var sb strings.Builder
			sb.WriteString(text)
			if len(citations) > 0 {
				sb.WriteString("\n\n---\n**Nguồn tham khảo:**\n")
				for i, c := range citations {
					fmt.Fprintf(&sb, "%d. [%s](%s)\n", i+1, c.Title, c.URL)
				}
			}
			return sb.String()
		},
	}
}
