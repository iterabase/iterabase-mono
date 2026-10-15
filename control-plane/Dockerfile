# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32

FROM node:24-alpine@sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf AS ui-builder
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# Build stage: compile all four binaries (CGO-free) with version LDFLAGS.
FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder
RUN apk add --no-cache git
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /ui/dist ./ui/dist

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ENV LDFLAGS="-X github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version.version=${VERSION} -X github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version.commit=${COMMIT} -X github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version.date=${DATE}"

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/manager ./cmd/manager && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/dispatch ./cmd/dispatch && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/artifact-coordinator ./cmd/artifact-coordinator && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/artifact-scanner ./cmd/artifact-scanner

# Runtime stage: one image, six binaries. Each Deployment selects its binary
# via `command` (manager: ["/manager"]; api: ["/api", "serve"]; gateway: ["/gateway", "serve"]; dispatch: ["/dispatch", "serve"];
# artifact-coordinator: ["/artifact-coordinator", "serve"]; artifact-scanner: ["/artifact-scanner"]).
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/manager /manager
COPY --from=builder /out/api /api
COPY --from=builder /out/gateway /gateway
COPY --from=builder /out/dispatch /dispatch
COPY --from=builder /out/artifact-coordinator /artifact-coordinator
COPY --from=builder /out/artifact-scanner /artifact-scanner
USER 65532:65532
