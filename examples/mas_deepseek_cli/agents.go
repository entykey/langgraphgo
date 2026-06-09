package main

import (
	"context"
	"fmt"
	"time"

	"github.com/smallnest/langgraphgo/graph"
)

const maxSteps = 10

// supervisorNode returns the langgraphgo node function for the supervisor.
func supervisorNode(ds *DeepSeekClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		t0 := time.Now()
		state.Step++

		fmt.Printf("\n╔═══ STEP %d ═══════════════════════════════════════════╗\n", state.Step)
		fmt.Printf("  ┌─ [SUPERVISOR routing]\n")

		if state.CallCount == nil {
			state.CallCount = make(map[string]int)
		}

		if state.Step > maxSteps {
			fmt.Println("  ⚠️  maxSteps reached → force FINISH")
			elapsed := time.Since(t0)
			state.Records = append(state.Records, StepRecord{Step: state.Step, Agent: "supervisor(max)", Elapsed: elapsed})
			state.Next = "FINISH"
			fmt.Printf("  └─ done: %.2fs\n╚═══════════════════════════════════════════════════════╝\n", elapsed.Seconds())
			return state, nil
		}

		router, err := ds.RouteJSON(ctx, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("supervisor routing: %w", err)
		}

		decision := router.Next
		fmt.Printf("  │  → %q | %s\n", decision, router.Reasoning)

		if agentNodes[decision] && state.CallCount[decision] >= 1 {
			fmt.Printf("  ⚠️  anti-loop: %q ran %dx → FINISH\n", decision, state.CallCount[decision])
			decision = "FINISH"
		}

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

// routingEdge reads state.Next and returns the next graph node name.
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

// selfReplyNode returns the node for direct supervisor responses.
// Goes to END directly — prevents the loop bug where "self" re-enters supervisor.
func selfReplyNode(ds *DeepSeekClient) func(context.Context, AgentState) (AgentState, error) {
	return func(ctx context.Context, state AgentState) (AgentState, error) {
		t0 := time.Now()
		fmt.Printf("\n🤖 [Supervisor]\n")

		text, metrics, err := ds.StreamChat(ctx, chatSystem, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("self-reply: %w", err)
		}

		state.Messages = append(state.Messages, Message{Role: "model", Content: text, Name: "supervisor"})
		state.Records = append(state.Records, StepRecord{
			Step: state.Step, Agent: "supervisor(self)", Elapsed: elapsed,
			Tokens: metrics.Tokens, TTFT: metrics.TTFT, GenTime: metrics.GenTime,
		})
		logMetrics(elapsed, metrics)
		return state, nil
	}
}

// makeAgentNode returns a streaming specialist agent node.
func makeAgentNode(ds *DeepSeekClient, name string) func(context.Context, AgentState) (AgentState, error) {
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

		text, metrics, err := ds.StreamChat(ctx, prompt, state.Messages)
		elapsed := time.Since(t0)
		if err != nil {
			return state, fmt.Errorf("%s: %w", name, err)
		}

		state.Messages = append(state.Messages, Message{Role: "model", Content: text, Name: name})
		state.CallCount[name]++
		state.Records = append(state.Records, StepRecord{
			Step: state.Step, Agent: name, Elapsed: elapsed,
			Tokens: metrics.Tokens, TTFT: metrics.TTFT, GenTime: metrics.GenTime,
		})
		logMetrics(elapsed, metrics)
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
