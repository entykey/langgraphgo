# syntax=docker/dockerfile:1
# Single Agent Harness — Go binary image
#
# Build context: langgraphgo/ (repo root — NOT examples/)
#
#   docker build \
#     -f examples/single_agent_harness/Dockerfile \
#     -t sah:latest .
#
# Run (with a .env file):
#   docker run --rm -p 8080:8080 \
#     --env-file examples/single_agent_harness/.env \
#     sah:latest

# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# examples/ uses its own go.mod with `replace github.com/smallnest/langgraphgo => ../`
# Both go.mod / go.sum files must be present before downloading deps.
COPY go.mod go.sum ./
COPY examples/go.mod examples/go.sum ./examples/

# Download dependencies — separate layer so source changes don't invalidate this.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd examples && go mod download

# Copy all source (including the parent module that examples/ replaces).
COPY . .

# Build static binary — single_agent_harness has no CGo deps.
WORKDIR /src/examples/single_agent_harness
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o /out/sah .

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21

# ca-certificates: HTTPS to DeepSeek / Gemini / Langfuse
# tzdata: correct time zones in logs
# docker-cli: needed to spawn the code-execution sandbox container
# Note: runs as root so it can access the Docker socket mounted at runtime.
RUN apk add --no-cache ca-certificates tzdata docker-cli

WORKDIR /app

COPY --from=builder /out/sah             ./sah
COPY examples/single_agent_harness/static ./static
COPY examples/single_agent_harness/skills  ./skills

# Upload dir for session file uploads (mounted as a volume in production).
RUN mkdir -p /app/tmp/uploads

EXPOSE 8080

# UPLOAD_DIR tells the agent where to store session uploads inside the container.
ENV UPLOAD_DIR=/app/tmp/uploads

# Health-check: hit the lightweight /config endpoint.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/config > /dev/null || exit 1

ENTRYPOINT ["/app/sah"]
