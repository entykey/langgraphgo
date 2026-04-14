// mas_gemini_cli — Multi-Agent System CLI using LangGraphGo + Gemini 2.0 Flash
//
// Graph topology:
//
//	START → supervisor
//	supervisor ──(conditional)──> web_search | code_expert | data_analyst |
//	                               security_expert | writing_expert | devops_expert |
//	                               self_reply | END
//	web_search        → supervisor
//	code_expert       → supervisor
//	data_analyst      → supervisor
//	security_expert   → supervisor
//	writing_expert    → supervisor
//	devops_expert     → supervisor
//	self_reply        → END          ← fixes the loop bug: self never re-enters supervisor
//
// Usage:
//
//	export GOOGLE_API_KEY=xxx
//	go run ./examples/mas_gemini_cli/
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/smallnest/langgraphgo/graph"
)

// loadDotEnvFile reads KEY=VALUE pairs from a single file and sets env vars
// (only when the variable is not already set in the environment).
// Returns true if the file was found and read.
func loadDotEnvFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v) //nolint:errcheck
		}
	}
	return true
}

// loadDotEnv tries to load a .env file from several candidate locations so the
// program works regardless of which directory it is run from:
//
//  1. Current working directory  (./.env)
//  2. Directory of the executable (e.g. bin/examples/.env)
//  3. Up to 4 parent directories  (../../.env, etc.) — useful when running
//     with `go run ./examples/mas_gemini_cli/` from the repo root
func loadDotEnv() {
	// 1. current working directory
	if cwd, err := os.Getwd(); err == nil {
		if loadDotEnvFile(filepath.Join(cwd, ".env")) {
			return
		}
	}

	// 2. directory of the running executable
	if exe, err := os.Executable(); err == nil {
		if loadDotEnvFile(filepath.Join(filepath.Dir(exe), ".env")) {
			return
		}
	}

	// 3. walk up from cwd looking for .env (handles `go run` from repo root)
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for i := 0; i < 4; i++ {
			parent := filepath.Dir(dir)
			if parent == dir {
				break // reached filesystem root
			}
			dir = parent
			if loadDotEnvFile(filepath.Join(dir, ".env")) {
				return
			}
		}
	}
}

// requireAPIKey returns GOOGLE_API_KEY or exits with an error message.
func requireAPIKey() string {
	k := os.Getenv("GOOGLE_API_KEY")
	if k == "" {
		fmt.Fprintln(os.Stderr, "❌  GOOGLE_API_KEY not set. Create a .env file or export the variable.")
		os.Exit(1)
	}
	return k
}

// buildGraph constructs and compiles the multi-agent langgraphgo graph.
func buildGraph(gemini *GeminiClient) (*graph.StateRunnable[AgentState], error) {
	g := graph.NewStateGraph[AgentState]()

	// ── nodes ────────────────────────────────────────────────────────────────

	g.AddNode("supervisor", "route to specialist agents", supervisorNode(gemini))
	g.AddNode("self_reply", "supervisor direct response", selfReplyNode(gemini))
	g.AddNode("web_search", "Google Search grounding", webSearchNode(gemini))

	// specialist streaming agents
	for name := range agentNodes {
		if name == "web_search" {
			continue // already registered above
		}
		n := name // capture loop variable for closure
		g.AddNode(n, n+" specialist", makeAgentNode(gemini, n))
	}

	// ── entry point ───────────────────────────────────────────────────────────
	g.SetEntryPoint("supervisor")

	// ── edges ─────────────────────────────────────────────────────────────────

	// supervisor → conditional routing (reads state.Next)
	g.AddConditionalEdge("supervisor", routingEdge)

	// self_reply goes directly to END — prevents the loop bug where "self" would
	// re-enter supervisor and potentially be called 6+ times per turn.
	g.AddEdge("self_reply", graph.END)

	// all specialists loop back to supervisor for the next routing decision
	g.AddEdge("web_search", "supervisor")
	for name := range agentNodes {
		if name == "web_search" {
			continue
		}
		g.AddEdge(name, "supervisor")
	}

	return g.Compile()
}

// printTimingTable renders the per-step timing summary after each user turn.
func printTimingTable(records []StepRecord, total time.Duration) {
	fmt.Println("\n┌───────────────────────────────────────────────────────┐")
	fmt.Println("│  📊 TIMING SUMMARY                                    │")
	fmt.Println("├──────┬──────────────────────────┬─────────────────────┤")
	fmt.Println("│ Step │ Agent                    │ Elapsed             │")
	fmt.Println("├──────┼──────────────────────────┼─────────────────────┤")
	for _, r := range records {
		fmt.Printf("│  %2d  │ %-24s │ %15.2fs     │\n", r.Step, r.Agent, r.Elapsed.Seconds())
	}
	fmt.Println("├──────┴──────────────────────────┴─────────────────────┤")
	fmt.Printf("│  TOTAL                                  %10.2fs     │\n", total.Seconds())
	fmt.Println("└───────────────────────────────────────────────────────┘")
}

func main() {
	loadDotEnv()
	apiKey := requireAPIKey()

	gemini := NewGeminiClient(apiKey)

	app, err := buildGraph(gemini)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ build graph: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(`
╔════════════════════════════════════════════════════════════╗
║  🤖  MAS-LangGraph — Multi-Agent System CLI                ║
║  Gemini 2.0 Flash · LangGraphGo · Multi-step               ║
╠════════════════════════════════════════════════════════════╣
║  Agents: web_search · code_expert · data_analyst           ║
║          security_expert · writing_expert · devops_expert  ║
╠════════════════════════════════════════════════════════════╣
║  'exit' thoát  |  'clear' xóa lịch sử  |  'help' mẫu      ║
╚════════════════════════════════════════════════════════════╝

💡 Thử: "Phiên bản Claude mới nhất?" | "Viết Dockerfile FastAPI" | "Security audit Flask"
`)

	ctx := context.Background()
	var history []Message

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	for {
		fmt.Print("\n👤 Bạn: ")
		if !sc.Scan() {
			break
		}
		input := strings.TrimSpace(sc.Text())
		if input == "" {
			continue
		}

		switch strings.ToLower(input) {
		case "exit", "quit":
			fmt.Println("👋 Tạm biệt!")
			return
		case "clear":
			history = nil
			fmt.Println("🗑️  Đã xóa lịch sử.")
			continue
		case "help":
			fmt.Println(`
Câu hỏi mẫu:
  • "Xin chào"                                         → self (1 bước)
  • "Phiên bản Claude mới nhất là gì?"                 → web_search
  • "Tìm tài liệu Tang Nano 4K và viết code blink LED" → web_search → code_expert
  • "Viết Dockerfile cho FastAPI"                       → devops_expert
  • "Security audit Flask login form"                   → security_expert`)
			continue
		}

		history = append(history, Message{Role: "user", Content: input})

		// Build fresh per-turn state. CallCount starts empty each turn so
		// the anti-loop tracks calls within this turn only.
		initialState := AgentState{
			Messages:   append([]Message(nil), history...), // defensive copy
			CallCount:  make(map[string]int),
			UserMsgIdx: len(history) - 1,
		}

		fmt.Println(strings.Repeat("─", 60))
		turnStart := time.Now()

		finalState, err := app.Invoke(ctx, initialState)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Lỗi: %v\n", err)
			history = history[:len(history)-1] // rollback user message
			continue
		}

		printTimingTable(finalState.Records, time.Since(turnStart))

		// Persist the full message history (includes all agent responses) for
		// the next turn so agents can reference prior context.
		history = finalState.Messages
	}
}
