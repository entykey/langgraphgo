// mas_deepseek_cli — Multi-Agent System CLI using LangGraphGo + DeepSeek v4-Flash
//
// Graph topology:
//
//	START → supervisor
//	supervisor ──(conditional)──> code_expert | data_analyst |
//	                               security_expert | writing_expert | devops_expert |
//	                               self_reply | END
//	all specialists → supervisor   (loop for multi-step tasks)
//	self_reply      → END          (prevents loop on direct answers)
//
// Usage:
//
//	export DEEPSEEK_API_KEY=xxx
//	go run ./examples/mas_deepseek_cli/
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

func loadDotEnv() {
	if cwd, err := os.Getwd(); err == nil {
		if loadDotEnvFile(filepath.Join(cwd, ".env")) {
			return
		}
	}
	if exe, err := os.Executable(); err == nil {
		if loadDotEnvFile(filepath.Join(filepath.Dir(exe), ".env")) {
			return
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for i := 0; i < 4; i++ {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
			if loadDotEnvFile(filepath.Join(dir, ".env")) {
				return
			}
		}
	}
}

func requireAPIKey() string {
	k := os.Getenv("DEEPSEEK_API_KEY")
	if k == "" {
		fmt.Fprintln(os.Stderr, "❌  DEEPSEEK_API_KEY not set. Create a .env file or export the variable.")
		os.Exit(1)
	}
	return k
}

func buildGraph(ds *DeepSeekClient) (*graph.StateRunnable[AgentState], error) {
	g := graph.NewStateGraph[AgentState]()

	g.AddNode("supervisor", "route to specialist agents", supervisorNode(ds))
	g.AddNode("self_reply", "supervisor direct response", selfReplyNode(ds))

	for name := range agentNodes {
		n := name
		g.AddNode(n, n+" specialist", makeAgentNode(ds, n))
	}

	g.SetEntryPoint("supervisor")
	g.AddConditionalEdge("supervisor", routingEdge)
	g.AddEdge("self_reply", graph.END)
	for name := range agentNodes {
		g.AddEdge(name, "supervisor")
	}

	return g.Compile()
}

// printTimingTable renders the per-turn timing + token metrics summary.
// Table is 72 chars wide.
func printTimingTable(records []StepRecord, total time.Duration) {
	const (
		top  = "┌──────────────────────────────────────────────────────────────────────┐"
		sep1 = "├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤"
		sep2 = "├──────┴────────────────────────┴─────────┴─────────┴───────┴──────────┤"
		bot  = "└──────────────────────────────────────────────────────────────────────┘"
	)

	fmt.Println("\n" + top)
	fmt.Printf("│  %-68s│\n", "TIMING SUMMARY — DeepSeek v4-Flash")
	fmt.Println(sep1)
	fmt.Printf("│ %-4s │ %-22s │ %-7s │ %-7s │ %-5s │ %-8s │\n",
		"Step", "Agent", "Elapsed", "TTFT", "Tok", "Tok/s")
	fmt.Println(sep1)

	var totalTokens int
	var totalGenTime time.Duration

	for _, r := range records {
		elapsed := fmt.Sprintf("%.2fs", r.Elapsed.Seconds())
		ttft, tok, tokS := "—", "—", "—"
		if r.Tokens > 0 {
			ttft = fmt.Sprintf("%dms", r.TTFT.Milliseconds())
			tok = fmt.Sprintf("%d", r.Tokens)
			tokS = fmt.Sprintf("%.1f", float64(r.Tokens)/r.GenTime.Seconds())
			totalTokens += r.Tokens
			totalGenTime += r.GenTime
		}
		fmt.Printf("│ %4d │ %-22s │ %7s │ %7s │ %5s │ %8s │\n",
			r.Step, r.Agent, elapsed, ttft, tok, tokS)
	}

	fmt.Println(sep2)

	avgTokS := 0.0
	if totalGenTime > 0 {
		avgTokS = float64(totalTokens) / totalGenTime.Seconds()
	}
	summary := fmt.Sprintf("  TOTAL %-7s · %d tok  · avg %.1f tok/s",
		fmt.Sprintf("%.2fs", total.Seconds()), totalTokens, avgTokS)
	fmt.Printf("│%-70s│\n", summary)
	fmt.Println(bot)
}

func main() {
	loadDotEnv()
	apiKey := requireAPIKey()

	ds := NewDeepSeekClient(apiKey)

	app, err := buildGraph(ds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ build graph: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(`
╔════════════════════════════════════════════════════════════╗
║  🤖  MAS-LangGraph — Multi-Agent System CLI                ║
║  DeepSeek v4-Flash · LangGraphGo · Multi-step              ║
╠════════════════════════════════════════════════════════════╣
║  Agents: code_expert · data_analyst · security_expert      ║
║          writing_expert · devops_expert                    ║
╠════════════════════════════════════════════════════════════╣
║  'exit' thoát  |  'clear' xóa lịch sử  |  'help' mẫu      ║
╚════════════════════════════════════════════════════════════╝

💡 Thử: "Viết Dockerfile FastAPI" | "Security audit Flask" | "Xin chào"
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
  • "Viết Dockerfile cho FastAPI"                       → devops_expert
  • "Security audit Flask login form"                   → security_expert
  • "Phân tích dataset CSV với pandas"                  → data_analyst
  • "Viết blog về Golang concurrency"                   → writing_expert
  • "Tìm tài liệu rồi viết code blink LED cho STM32"   → code_expert (multi-step)`)
			continue
		}

		history = append(history, Message{Role: "user", Content: input})

		initialState := AgentState{
			Messages:   append([]Message(nil), history...),
			CallCount:  make(map[string]int),
			UserMsgIdx: len(history) - 1,
		}

		fmt.Println(strings.Repeat("─", 60))
		turnStart := time.Now()

		finalState, err := app.Invoke(ctx, initialState)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Lỗi: %v\n", err)
			history = history[:len(history)-1]
			continue
		}

		printTimingTable(finalState.Records, time.Since(turnStart))
		history = finalState.Messages
	}
}
