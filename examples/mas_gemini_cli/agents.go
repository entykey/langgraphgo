package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/langgraphgo/graph"
)

const maxSteps = 10

// supervisorNode returns the langgraphgo node function for the supervisor.
//
// Responsibilities:
//   - Increment the step counter
//   - Call Gemini with JSON mode to get a routing decision
//   - Apply anti-loop guard (if a specialist already ran, force FINISH)
//   - Apply FINISH guard (if no model response exists yet, redirect to self)
//   - Write the decision into state.Next (read by the conditional edge)
func supervisorNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		t0 := time.Now()
		state.Step++

		fmt.Printf("\n╔═══ STEP %d ═══════════════════════════════════════════╗\n", state.Step)
		fmt.Printf("  ┌─ [SUPERVISOR routing]\n")

		// ensure CallCount is initialised (safety net for first call)
		if state.CallCount == nil {
			state.CallCount = make(map[string]int)
		}

		// hard step limit — prevents runaway graphs
		if state.Step > maxSteps {
			fmt.Println("  ⚠️  maxSteps reached → force FINISH")
			elapsed := time.Since(t0)
			state.Records = append(state.Records, StepRecord{Step: state.Step, Agent: "supervisor(max)", Elapsed: elapsed})
			state.Next = "FINISH"
			fmt.Printf("  └─ done: %.2fs\n╚═══════════════════════════════════════════════════════╝\n", elapsed.Seconds())
			return state, nil
		}

		router, err := gemini.RouteJSON(ctx, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("supervisor routing: %w", err)
		}

		decision := router.Next
		hint := router.Reasoning
		// if len(hint) > 100 {
		// 	hint = hint[:100] + "..."
		// }
		fmt.Printf("  │  → %q | %s\n", decision, hint)

		// ── Anti-loop ────────────────────────────────────────────────────────
		// If a specialist has already been called once this turn, forbid calling
		// it again to prevent the graph from cycling indefinitely.
		if agentNodes[decision] && state.CallCount[decision] >= 1 {
			fmt.Printf("  ⚠️  anti-loop: %q ran %dx → FINISH\n", decision, state.CallCount[decision])
			decision = "FINISH"
		}

		// ── FINISH guard ─────────────────────────────────────────────────────
		// If the supervisor says FINISH but no model response exists after the
		// latest user message, force a self-reply instead of ending silently.
		if decision == "FINISH" {
			hasResp := false
			for _, m := range state.Messages[state.UserMsgIdx+1:] {
				if m.Role == "model" {
					hasResp = true
					break
				}
			}
			if hasResp {
				fmt.Println("  ✅ FINISH")
			} else {
				fmt.Println("  ⚠️  FINISH without response → self")
				decision = "self"
			}
		}

		state.Next = decision
		state.Records = append(state.Records, StepRecord{Step: state.Step, Agent: "supervisor→route", Elapsed: elapsed})
		fmt.Printf("  └─ done: %.2fs\n╚═══════════════════════════════════════════════════════╝\n", elapsed.Seconds())
		return state, nil
	}
}

// routingEdge is the conditional edge function attached to the supervisor node.
// It reads state.Next and returns the name of the next node (or graph.END).
//
// Topology:
//
//	supervisor ──(conditional)──> web_search | code_expert | ... | self_reply | END
func routingEdge(_ context.Context, state AgentState) string {
	switch state.Next {
	case "FINISH", "":
		return graph.END
	case "self":
		return "self_reply"
	default:
		return state.Next
	}
}

// selfReplyNode returns the node function for direct supervisor responses.
//
// This node goes directly to END (not back to supervisor), which is the fix
// for the loop bug in the reference implementation where "self" could be called
// 6+ times per turn.
func selfReplyNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		t0 := time.Now()
		fmt.Printf("\n🤖 [Supervisor]\n")

		text, metrics, err := gemini.StreamChat(ctx, chatSystem, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("self-reply: %w", err)
		}

		state.Messages = append(state.Messages, Message{
			Role:    "model",
			Content: text,
			Name:    "supervisor",
		})
		state.Records = append(state.Records, StepRecord{
			Step: state.Step, Agent: "supervisor(self)", Elapsed: elapsed,
			Tokens: metrics.Tokens, TTFT: metrics.TTFT, GenTime: metrics.GenTime,
		})
		logMetrics(elapsed, metrics)
		return state, nil
	}
}

// webSearchNode returns the node function for Google Search grounding.
// Uses non-streaming generateContent with the googleSearch tool, then
// formats and prints citations from grounding_metadata.
func webSearchNode(gemini *GeminiClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		t0 := time.Now()
		fmt.Printf("\n🔍 [Web Search] đang tìm...\n")

		if state.CallCount == nil {
			state.CallCount = make(map[string]int)
		}

		fullText, citations, err := gemini.WebSearch(ctx, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("web_search: %w", err)
		}

		fmt.Println()
		fmt.Println(fullText)

		// Append citations block to the stored message so downstream agents
		// (e.g. code_expert) can reference them in the conversation history.
		stored := fullText
		if len(citations) > 0 {
			fmt.Println("\n─── Nguồn ─────────────────────────────────────────────")
			var srcLines []string
			for i, cit := range citations {
				fmt.Printf("  [%d] %s\n      %s\n", i+1, cit.Title, cit.URL)
				srcLines = append(srcLines, fmt.Sprintf("%d. [%s](%s)", i+1, cit.Title, cit.URL))
			}
			fmt.Println("────────────────────────────────────────────────────────")
			stored = fullText + "\n\n---\n**Nguồn:**\n" + strings.Join(srcLines, "\n")
		}

		state.Messages = append(state.Messages, Message{
			Role:    "model",
			Content: stored,
			Name:    "web_search",
		})
		state.CallCount["web_search"]++
		state.Records = append(state.Records, StepRecord{Step: state.Step, Agent: "web_search", Elapsed: elapsed})
		fmt.Printf("  done: %.2fs\n", elapsed.Seconds())
		return state, nil
	}
}

func logMetrics(elapsed time.Duration, m StreamMetrics) {
	if m.Tokens > 0 {
		fmt.Printf("  done: %.2fs  |  TTFT: %dms  |  gen: %.2fs  |  %d tok  |  %.1f tok/s\n",
			elapsed.Seconds(), m.TTFT.Milliseconds(), m.GenTime.Seconds(), m.Tokens, m.TokPerSec)
	} else {
		fmt.Printf("  done: %.2fs\n", elapsed.Seconds())
	}
}

// makeAgentNode returns the node function for a streaming specialist agent.
// All specialists (code_expert, data_analyst, etc.) share this factory.
func makeAgentNode(gemini *GeminiClient, name string) func(context.Context, AgentState) (AgentState, error) {
	emoji := agentEmojis[name]
	if emoji == "" {
		emoji = "🤖"
	}
	prompt := agentPrompts[name]

	return func(ctx context.Context, state AgentState) (AgentState, error) {
		t0 := time.Now()
		fmt.Printf("\n%s [%s]\n", emoji, name)

		if state.CallCount == nil {
			state.CallCount = make(map[string]int)
		}

		text, metrics, err := gemini.StreamChat(ctx, prompt, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("%s: %w", name, err)
		}

		state.Messages = append(state.Messages, Message{
			Role:    "model",
			Content: text,
			Name:    name,
		})
		state.CallCount[name]++
		state.Records = append(state.Records, StepRecord{
			Step: state.Step, Agent: name, Elapsed: elapsed,
			Tokens: metrics.Tokens, TTFT: metrics.TTFT, GenTime: metrics.GenTime,
		})
		logMetrics(elapsed, metrics)
		return state, nil
	}
}
