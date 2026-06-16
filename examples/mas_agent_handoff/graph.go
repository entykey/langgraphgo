package main

import (
	"context"
	"os"
	"strings"

	"github.com/smallnest/langgraphgo/graph"
)

// buildGraph constructs the supervisor → (json_agent | vision_agent | END) graph.
// web_search is no longer a standalone node — it is a tool inside the supervisor's
// self-reply ReAct loop so the supervisor synthesises the final answer itself.
//
// SUPERVISOR_BACKEND env: "gemini" → Gemini Flash Lite; anything else → DeepSeek (default).
func buildGraph(gemini *GeminiClient, ds *DSClient) (*graph.StateRunnable[AgentState], error) {
	g := graph.NewStateGraph[AgentState]()

	var supervisorBackend SupervisorBackend
	if strings.EqualFold(os.Getenv("SUPERVISOR_BACKEND"), "gemini") {
		supervisorBackend = gemini
		supervisorModel = "gemini-3.1-flash-lite"
	} else {
		supervisorBackend = ds
		supervisorModel = "deepseek-v4-flash"
	}

	g.AddNode("supervisor", "route request or self-reply with tools", supervisorNode(supervisorBackend, gemini))
	g.AddNode("json_agent", "JSONPlaceholder data agent (DeepSeek)", jsonAgentNode(ds))
	g.AddNode("vision_agent", "Gemini VLM — image analysis", visionAgentNode(gemini))

	g.SetEntryPoint("supervisor")

	g.AddConditionalEdge("supervisor", func(_ context.Context, state AgentState) string {
		switch state.Next {
		case "json_agent", "vision_agent":
			return state.Next
		default:
			return graph.END
		}
	})

	g.AddEdge("json_agent", graph.END)
	g.AddEdge("vision_agent", graph.END)

	return g.Compile()
}
