package main

import (
	"context"
	"fmt"
	"time"
)

const visionAgentSystemPrompt = `Bạn là trợ lý phân tích hình ảnh thông minh.
Phân tích chi tiết nội dung ảnh và trả lời câu hỏi của người dùng bằng tiếng Việt.
Mô tả những gì bạn thấy, đọc text trong ảnh nếu có, và cung cấp thông tin hữu ích.`

// visionAgentNode calls Gemini with the uploaded image + user text (streaming).
// Only reachable when AgentState.ImageB64 is non-empty (supervisor routes here).
func visionAgentNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		emit(state.EventCh, "node_start", map[string]string{"node": "vision_agent"})
		fmt.Printf("[vision_agent] starting — mime=%s size~%dKB\n",
			state.ImageMime, len(state.ImageB64)*3/4/1024)

		lastUser := ""
		for i := len(state.Messages) - 1; i >= 0; i-- {
			if state.Messages[i].Role == "user" {
				lastUser = state.Messages[i].Content
				break
			}
		}

		genID := lfUUID()
		approxBytes := len(state.ImageB64) * 3 / 4
		globalLF.GenerationStart(genID, state.TraceID, "", "vision_agent", gemModel,
			[]map[string]any{
				{"role": "system", "content": visionAgentSystemPrompt},
				{"role": "user", "content": fmt.Sprintf("[image: %s, ~%d bytes] %s",
					state.ImageMime, approxBytes, lastUser)},
			})

		var ttft time.Time
		text, promptTok, completionTok, err := gemini.StreamChatWithImage(
			ctx,
			visionAgentSystemPrompt,
			state.Messages,
			state.ImageB64,
			state.ImageMime,
			func(tok string) {
				if ttft.IsZero() {
					ttft = time.Now()
				}
				emit(state.EventCh, "token", map[string]string{"text": tok})
			},
		)
		if err != nil {
			globalLF.GenerationEnd(genID, state.TraceID, map[string]any{"error": err.Error()}, 0, 0, time.Time{})
			return state, fmt.Errorf("vision_agent: %w", err)
		}

		globalLF.GenerationEnd(genID, state.TraceID,
			map[string]any{"text_preview": truncate(text, 300)},
			promptTok, completionTok, ttft)

		fmt.Printf("[vision_agent] done — %d prompt tok, %d completion tok\n", promptTok, completionTok)

		state.Messages = append(state.Messages, Message{
			Role:    "model",
			Content: text,
			Name:    "vision_agent",
		})
		return state, nil
	}
}
