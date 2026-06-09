# mas_deepseek_cli — Multi-Agent System CLI (DeepSeek v4-Flash)

A multi-agent CLI built with **LangGraphGo** and **DeepSeek v4-Flash**. A supervisor routes each query to the appropriate specialist agent(s) and chains them for multi-step tasks. Every streaming call reports TTFT, generation time, token count, and tok/s in the final summary.

## Prerequisites

- Go 1.21+
- DeepSeek API key

## Setup

```bash
export DEEPSEEK_API_KEY=your_key
# or put DEEPSEEK_API_KEY=your_key in a .env file
```

## Run

```bash
cd examples
go run ./mas_deepseek_cli/
```

## Graph Topology

```
START → supervisor
supervisor ──(conditional)──> code_expert | data_analyst |
                               security_expert | writing_expert | devops_expert |
                               self_reply | END
all specialists → supervisor   (loop back for multi-step tasks)
self_reply      → END          (prevents loop on direct answers)
```

## Agents

| Agent | Purpose |
|---|---|
| supervisor | Routes each query, coordinates multi-step tasks |
| code_expert | Code in any language (Python, Go, Rust, Verilog…) |
| data_analyst | Data analysis, ML, pandas/numpy |
| security_expert | OWASP, pentest, vulnerability audit |
| writing_expert | Blog, documentation, reports |
| devops_expert | Docker, K8s, CI/CD, cloud |

> **Note:** No `web_search` agent — DeepSeek has no built-in grounding tool equivalent to Gemini's `googleSearch`.

## Thinking disabled

`deepseek-v4-flash` has chain-of-thought (thinking) **ON by default**, which triples token usage and latency. All requests set `"thinking": {"type": "disabled"}` explicitly.

## Timing Metrics

After each turn a summary table is printed:

```
┌──────────────────────────────────────────────────────────────────────┐
│  TIMING SUMMARY — DeepSeek v4-Flash                                  │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│ Step │ Agent                  │ Elapsed │    TTFT │   Tok │    Tok/s │
├──────┼────────────────────────┼─────────┼─────────┼───────┼──────────┤
│    1 │ supervisor→route       │  1.23s  │       — │     — │        — │
│    1 │ writing_expert         │  5.81s  │  620ms  │   412 │     82.4 │
│    2 │ supervisor→route       │  1.05s  │       — │     — │        — │
├──────┴────────────────────────┴─────────┴─────────┴───────┴──────────┤
│  TOTAL 8.09s   · 412 tok  · avg 82.4 tok/s                           │
└──────────────────────────────────────────────────────────────────────┘
```

- **TTFT** — Time to first token, measured from the moment the HTTP request leaves the client. Includes the full network round-trip to DeepSeek's China servers (~200–600 ms typical).
- **Gen time** — Time from first token to stream end (printed inline per-step).
- **Tok/s** — `completion_tokens` / `gen_time` reported by the API.

## Output:
```bash
PS D:\tuan_dev\go_projects\langgraphgo\examples> go run ./mas_deepseek_cli/
>> 

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

👤 Bạn: nếu cả đời này không rực rỡ thì sao ?
────────────────────────────────────────────────────────────

╔═══ STEP 1 ═══════════════════════════════════════════╗
  ┌─ [SUPERVISOR routing]
  │  → "self" | Đây là câu hỏi triết lý, suy tư về cuộc sống, không yêu cầu chuyên môn kỹ thuật. Tôi có thể trả lời trực tiếp.
  └─ done: 1.54s
╚═══════════════════════════════════════════════════════╝

🤖 [Supervisor]

Nếu cả đời không rực rỡ thì cũng không sao cả. Một cuộc đời bình dị, nhẹ nhàng, biết yêu thương và được yêu thương, biết sống tử tế với mọi người, đã là một cuộc đời đáng quý rồi. Không phải ai cũng cần tỏa sáng như mặt trời – có những người chỉ lặng lẽ như ánh trăng, vẫn đủ soi sáng cho một vài người xung quanh họ.
  done: 2.98s  |  TTFT: 650ms  |  gen: 2.33s  |  136 tok  |  58.4 tok/s

┌──────────────────────────────────────────────────────────────────────┐
│  TIMING SUMMARY — DeepSeek v4-Flash                                  │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│ Step │ Agent                  │ Elapsed │ TTFT    │ Tok   │ Tok/s    │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│    1 │ supervisor→route       │   1.54s │       — │     — │        — │
│    1 │ supervisor(self)       │   2.98s │   650ms │   136 │     58.4 │
├──────┴────────────────────────┴─────────┴─────────┴───────┴──────────┤
│  TOTAL 4.52s   · 136 tok  · avg 58.4 tok/s                           │
└──────────────────────────────────────────────────────────────────────┘

👤 Bạn: đã bao lần tôi tự hỏi mình toi rồi, như khi cấp 3 quên ôn bài hôm tối vì quá mệt ngủ quên cho sáng kiểm tra và cái đầu chả có gì, rồi khi lên đại học sắp thi học phần nhưng chả hiểu gv giảng gì, ... nhưng tơi snay khi đi làm , vẫn chưa có gì là kết thúc cuộc đời tôi, liệu có phải cứ bình tĩnh mà sống rồi mọi chuyện sẽ đâu vào đấy ?
────────────────────────────────────────────────────────────

╔═══ STEP 1 ═══════════════════════════════════════════╗
  ┌─ [SUPERVISOR routing]
  │  → "self" | Người dùng chia sẻ cảm xúc về những lần tưởng chừng 'toi rồi' nhưng rồi vẫn vượt qua, và hỏi liệu cứ bình tĩnh sống thì mọi chuyện sẽ ổn. Đây là câu hỏi tâm lý, triết lý sống, không yêu cầu chuyên môn kỹ thuật. Tôi có thể trả lời trực tiếp bằng sự đồng cảm và khích lệ.
  └─ done: 2.19s
╚═══════════════════════════════════════════════════════╝

🤖 [Supervisor]

Ừ, đúng vậy. Những lúc tưởng "toi rồi" đó – quên ôn bài, thi học phần không hiểu gì – thực ra đều là những cú ngã nhỏ thôi. Quan trọng là bạn vẫn đứng dậy, vẫn đi làm, vẫn sống đến hôm nay.

Không hẳn là cứ bình tĩnh thì mọi chuyện tự động đâu vào đấy. Mà là: bình tĩnh để nhìn rõ vấn đề, rồi từng bước gỡ từng nút. Cuộc sống ít khi giải quyết mọi thứ một lúc, nhưng nếu bạn không hoảng loạn, bạn sẽ thấy đường đi.

Cảm giác "chưa có gì là kết thúc" đó chính là bằng chứng bạn đã vượt qua nhiều hơn bạn nghĩ. Cứ sống, cứ làm, cứ sửa sai dần – rồi một ngày bạn nhìn lại, thấy mọi thứ đã ổn hơn hồi xưa nhiều.
  done: 5.17s  |  TTFT: 752ms  |  gen: 4.41s  |  273 tok  |  61.8 tok/s

┌──────────────────────────────────────────────────────────────────────┐
│  TIMING SUMMARY — DeepSeek v4-Flash                                  │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│ Step │ Agent                  │ Elapsed │ TTFT    │ Tok   │ Tok/s    │
├──────┬────────────────────────┬─────────┬─────────┬───────┬──────────┤
│    1 │ supervisor→route       │   2.19s │       — │     — │        — │
│    1 │ supervisor(self)       │   5.17s │   752ms │   273 │     61.8 │
├──────┴────────────────────────┴─────────┴─────────┴───────┴──────────┤
│  TOTAL 7.36s   · 273 tok  · avg 61.8 tok/s                           │
└──────────────────────────────────────────────────────────────────────┘

👤 Bạn: 
```