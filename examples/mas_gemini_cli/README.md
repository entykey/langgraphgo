# mas_gemini_cli — Multi-Agent System CLI (Gemini 3.1 Flash Lite)

A multi-agent CLI built with **LangGraphGo** and **Gemini 3.1 Flash Lite**. A supervisor routes each user query to the appropriate specialist agent(s) and chains them for multi-step tasks.

## Prerequisites

- Go 1.21+
- Google API key with Gemini API access

## Setup

```bash
export GOOGLE_API_KEY=your_key
# or put GOOGLE_API_KEY=your_key in a .env file
```

## Run

```bash
cd examples
go run ./mas_gemini_cli/
```

## Graph Topology

```
START → supervisor
supervisor ──(conditional)──> web_search | code_expert | data_analyst |
                               security_expert | writing_expert | devops_expert |
                               self_reply | END
all specialists → supervisor   (loop back for multi-step tasks)
self_reply      → END          (prevents loop on direct answers)
```

## Agents

| Agent | Purpose |
|---|---|
| supervisor | Routes each query, coordinates multi-step tasks |
| web_search | Google Search grounding for up-to-date info |
| code_expert | Code in any language (Python, Go, Rust, Verilog…) |
| data_analyst | Data analysis, ML, pandas/numpy |
| security_expert | OWASP, pentest, vulnerability audit |
| writing_expert | Blog, documentation, reports |
| devops_expert | Docker, K8s, CI/CD, cloud |

## Example Output

```bash
PS D:\tuan_dev\go_projects\langgraphgo\examples> go run ./mas_gemini_cli/

╔════════════════════════════════════════════════════════════╗
║  🤖  MAS-LangGraph — Multi-Agent System CLI                ║
║  Gemini 3.1 Flash Lite · LangGraphGo · Multi-step          ║
╠════════════════════════════════════════════════════════════╣
║  Agents: web_search · code_expert · data_analyst           ║
║          security_expert · writing_expert · devops_expert  ║
╠════════════════════════════════════════════════════════════╣
║  'exit' thoát  |  'clear' xóa lịch sử  |  'help' mẫu      ║
╚════════════════════════════════════════════════════════════╝

💡 Thử: "Phiên bản Claude mới nhất?" | "Viết Dockerfile FastAPI" | "Security audit Flask"

👤 Bạn: nếu cả đời này không rực rỡ thì sao ?
────────────────────────────────────────────────────────────

╔═══ STEP 1 ═══════════════════════════════════════════╗
  ┌─ [SUPERVISOR routing]
  │  → "writing_expert" | Câu hỏi triết lý, cảm xúc — cần writing expert.
  └─ done: 1.15s
╚═══════════════════════════════════════════════════════╝

✍️  [writing_expert]

...response...
  done: 5.29s

╔═══ STEP 2 ═══════════════════════════════════════════╗
  ┌─ [SUPERVISOR routing]
  │  → "FINISH" | Response is complete.
  ✅ FINISH
  └─ done: 1.05s
╚═══════════════════════════════════════════════════════╝

┌───────────────────────────────────────────────────────┐
│  📊 TIMING SUMMARY                                    │
├──────┬──────────────────────────┬─────────────────────┤
│ Step │ Agent                    │ Elapsed             │
├──────┼──────────────────────────┼─────────────────────┤
│   1  │ supervisor→route         │            1.15s     │
│   1  │ writing_expert           │            5.29s     │
│   2  │ supervisor→route         │            1.05s     │
├──────┴──────────────────────────┴─────────────────────┤
│  TOTAL                                        7.49s     │
└───────────────────────────────────────────────────────┘
```
