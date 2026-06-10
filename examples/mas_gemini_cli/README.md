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
>> 

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
  │  → "self" | Câu hỏi mang tính triết lý và chiêm nghiệm về ý nghĩa cuộc sống, không yêu cầu kỹ thuật hay tra cứu, cần một câu trả lời mang tính đồng cảm và khai mở tư duy từ phía AI.
  └─ done: 1.14s
╚═══════════════════════════════════════════════════════╝

🤖 [Supervisor]

Nếu cả đời không rực rỡ, thì cũng chẳng sao cả.

Chúng ta thường bị áp lực phải trở nên "phi thường", nhưng thực tế, sự bình yên và an ổn mới là điều khó đạt được nhất. Một cuộc đời bình thường, sống trọn vẹn từng ngày, được làm điều mình thích và yêu thương những người xung quanh, đó đã là một thành tựu lớn rồi.

Rực rỡ hay không là do mình tự định nghĩa. Nếu bạn thấy vui với một tách cà phê ngon, một giấc ngủ ngon, thì đó đã là sự rực rỡ của riêng bạn rồi. Đừng sống để trở thành "ngôi sao" trong mắt người khác, hãy sống để thấy nhẹ lòng với chính mình là đủ.
  done: 1.12s  |  TTFT: 588ms  |  gen: 0.53s  |  156 tok  |  293.0 tok/s

┌──────────────────────────────────────────────────────────────────────┐
│  TIMING SUMMARY — Gemini 3.1 Flash Lite                              │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│ Step │ Agent                  │ Elapsed │ TTFT    │ Tok   │ Tok/s    │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│    1 │ supervisor→route       │   1.14s │       — │     — │        — │
│    1 │ supervisor(self)       │   1.12s │   588ms │   156 │    293.0 │
├──────┴────────────────────────┴─────────┴─────────┴───────┴──────────┤
│  TOTAL 2.26s   · 156 tok  · avg 293.0 tok/s                          │
└──────────────────────────────────────────────────────────────────────┘

👤 Bạn: 
```
