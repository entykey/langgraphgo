// end_conv_eval — CLI test harness for the end_conversation prompt behaviour.
//
// Usage:
//
//	go run . [flags]
//
// Flags:
//
//	-thinking   enable DeepSeek thinking mode (slower, ~3× TTFT)
//	-case STR   run only cases whose name contains STR
//	-v          print full model responses per turn
//
// API key: DEEPSEEK_API_KEY env var, or read from the first .env file found
// at ./.env, ../.env, or ../single_agent_harness/.env.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	thinking    := flag.Bool("thinking", false, "enable DeepSeek thinking mode (slower, ~3× TTFT)")
	filter      := flag.String("case", "", "run only cases whose name contains this substring")
	verbose     := flag.Bool("v", false, "print full model responses per turn")
	judgePrompt := flag.Bool("judge-prompt", false, "after run, print full transcripts + eval criteria for LLM judge")
	outputFile  := flag.String("output", "", "write report (and judge prompt) to this UTF-8 file instead of stdout")
	flag.Parse()

	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		key = readEnvKey("DEEPSEEK_API_KEY",
			".env",
			"../.env",
			"../single_agent_harness/.env",
		)
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "error: DEEPSEEK_API_KEY not set (env var or .env file)")
		os.Exit(1)
	}

	c := newClient(key, "deepseek-v4-flash", *thinking)

	thinkStr := "off"
	if *thinking {
		thinkStr = "on (reasoning_effort: high)"
	}
	fmt.Printf("\n══════════════════════════════════════════════════════════\n")
	fmt.Printf(" end_conversation eval  ·  model: deepseek-v4-flash\n")
	fmt.Printf(" thinking: %s\n", thinkStr)
	fmt.Printf("══════════════════════════════════════════════════════════\n\n")

	results := runSuite(c, *filter, *verbose)

	var out io.Writer = os.Stdout
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot create output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	printReport(out, results)

	if *judgePrompt {
		printJudgePrompt(out, results)
	}

	for _, r := range results {
		if r.status != "PASS" {
			os.Exit(1)
		}
	}
}

// readEnvKey reads a key from the first .env file in paths that contains it.
func readEnvKey(key string, paths ...string) string {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == key {
				return strings.Trim(strings.TrimSpace(parts[1]), `"'`)
			}
		}
	}
	return ""
}
