package main

import (
	"context"
	"os"
	"strings"

	"github.com/smallnest/langgraphgo/graph"
)

// buildGraph constructs the supervisor → (json_agent | web_search | END) graph.
// SUPERVISOR_BACKEND env: "gemini" → Gemini Flash Lite; anything else → DeepSeek (default).
func buildGraph(gemini *GeminiClient, ds *DSClient) (*graph.StateRunnable[AgentState], error) {
	g := graph.NewStateGraph[AgentState]()

	var supervisorBackend SupervisorBackend
	if strings.EqualFold(os.Getenv("SUPERVISOR_BACKEND"), "deepseek") {
		supervisorBackend = ds
		supervisorModel = "deepseek-v4-flash"
	} else {
		supervisorBackend = gemini
		supervisorModel = "gemini-3.1-flash-lite"
	}

	g.AddNode("supervisor", "route request to the correct agent", supervisorNode(supervisorBackend))
	g.AddNode("json_agent", "JSONPlaceholder data agent (DeepSeek)", jsonAgentNode(ds))
	g.AddNode("web_search", "Google Search grounding (Gemini)", webSearchNode(gemini))

	g.SetEntryPoint("supervisor")

	// Supervisor sets state.Next; this edge routes to the chosen node or ends the graph.
	g.AddConditionalEdge("supervisor", func(_ context.Context, state AgentState) string {
		switch state.Next {
		case "json_agent", "web_search":
			return state.Next
		default:
			return graph.END
		}
	})

	g.AddEdge("json_agent", graph.END)
	g.AddEdge("web_search", graph.END)

	return g.Compile()
}
