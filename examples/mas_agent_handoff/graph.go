package main

import (
	"context"

	"github.com/smallnest/langgraphgo/graph"
)

// buildGraph constructs the supervisor → (json_agent | web_search | END) graph.
func buildGraph(gemini *GeminiClient, ds *DSClient) (*graph.StateRunnable[AgentState], error) {
	g := graph.NewStateGraph[AgentState]()

	g.AddNode("supervisor", "route request to the correct agent", supervisorNode(gemini))
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
