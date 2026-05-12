# Multi-stage build for the teem-worker daemon. Stage 1 builds the static
# Go binary; stage 2 is a slim runtime that also has the Claude Code CLI
# installed (it shells out to `claude -p ...` for each job).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/teem-worker ./cmd/teem-worker

FROM node:22-alpine
RUN apk add --no-cache ca-certificates tini \
    && npm install -g @anthropic-ai/claude-code \
    && addgroup -S teem && adduser -S -G teem teem \
    && mkdir -p /workspace && chown teem:teem /workspace
COPY --from=build /out/teem-worker /usr/local/bin/teem-worker
USER teem
WORKDIR /workspace
ENV TEEM_LISTEN_PORT=:7780 \
    TEEM_WORKER_WORKDIR=/workspace
EXPOSE 7780
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/teem-worker"]
