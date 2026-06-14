# MAS Agent Handoff

Multi-agent system with supervisor routing, streaming SSE, image analysis, and Langfuse observability. Built with Go + LangGraphGo + Vite.

## Architecture

```
┌─────────────────── docker compose ────────────────────────┐
│  Langfuse (3100)  Postgres (5533)  ClickHouse  Redis      │
│  MinIO API (9000)  MinIO Console (9093)                    │
└───────────────────────────────────────────────────────────┘
           ↕ MINIO_ENDPOINT=localhost:9000
           ↕ LANGFUSE_BASE_URL=http://localhost:3100
┌───────────────── host ────────────────────────────────────┐
│  go run ./mas_agent_handoff/   →  http://localhost:8080   │
└───────────────────────────────────────────────────────────┘
```

**Agents:**
| Agent | Model | Trigger |
|---|---|---|
| Supervisor | DeepSeek v4 Flash | every turn — routes request |
| JSON Agent | DeepSeek v4 Flash + tools | JSONPlaceholder data queries |
| Web Search | Gemini Flash Lite + Google Search | live web / news |
| Vision | Gemini Flash Lite (VLM) | image attached |
| Self-reply | Supervisor (inline) | greeting / small talk |

**MinIO** is shared: Langfuse uses the `langfuse` bucket internally; the Go backend stores uploaded images in `mas-images`. Both share the same MinIO instance in the compose stack.

---

## Prerequisites

- [Docker + Docker Compose](https://docs.docker.com/get-docker/)
- [Go 1.21+](https://go.dev/dl/)
- Node.js 18+ (only needed to rebuild the frontend)

---

## Quickstart

### 1. Fill in API keys

Edit `.env` — the keys you need:

```bash
GOOGLE_API_KEY=...       # Gemini (vision + web search)
DEEPSEEK_API_KEY=...     # DeepSeek (supervisor + JSON agent)
```

Everything else (infra passwords, MinIO creds) is pre-filled with defaults that match the compose file.

### 2. Start infra

```bash
docker compose up -d
```

Wait ~30 s for all services to become healthy:

```bash
docker compose ps   # all should show "healthy" or "running"
```

### 3. Create a Langfuse project

Open <http://localhost:3100>, register, create a project, then copy the **Public** and **Secret** keys into `.env`:

```bash
LANGFUSE_PUBLIC_KEY=pk-lf-...
LANGFUSE_SECRET_KEY=sk-lf-...
```

> Skip this step if you already have keys in `.env` from a previous run.

### 4. Build the frontend (first time only)

```bash
cd examples/mas_agent_handoff/fe
npm install
npm run build
```

The Go server automatically serves `fe/dist/` at `/`.

### 5. Run the Go backend

```bash
cd examples
go run ./mas_agent_handoff/
```

Open <http://localhost:8080>.

---

## Ports

| Port | Service | Note |
|---|---|---|
| 8080 | Go backend | API + SPA |
| 3100 | Langfuse UI | traces + analytics |
| 9000 | MinIO S3 API | Go backend connects here |
| 9093 | MinIO console | web UI |
| 5533 | Postgres | optional direct access |

---

## Image uploads

Uploaded images are stored in MinIO bucket `mas-images` (persistent across restarts).  
Fallback: in-memory base64 if `MINIO_ENDPOINT` is unset.

```
POST /upload          → { image_id, mime, size_kb, backend }
GET  /image/{uuid}    → image bytes (streamed from MinIO)
```

---

## Env reference

| Key | Default | Description |
|---|---|---|
| `GOOGLE_API_KEY` | — | **required** |
| `DEEPSEEK_API_KEY` | — | **required** |
| `LANGFUSE_PUBLIC_KEY` | — | from Langfuse project |
| `LANGFUSE_SECRET_KEY` | — | from Langfuse project |
| `LANGFUSE_BASE_URL` | `http://localhost:3100` | |
| `MINIO_ENDPOINT` | `localhost:9000` | |
| `MINIO_ACCESS_KEY` | `minio` | matches compose `MINIO_ROOT_USER` |
| `MINIO_SECRET_KEY` | `mas_minio_pw` | matches `MAS_MINIO_PASSWORD` |
| `MINIO_BUCKET` | `mas-images` | created automatically |
| `SUPERVISOR_PROVIDER` | `deepseek` | `deepseek` or `gemini` |

---

## Frontend dev mode (hot reload)

```bash
cd examples/mas_agent_handoff/fe
npm run dev   # http://localhost:5173, proxies /chat /upload to :8080
```

---

## Stop / clean up

```bash
docker compose down          # stop containers, keep volumes
docker compose down -v       # stop + delete all data
```
