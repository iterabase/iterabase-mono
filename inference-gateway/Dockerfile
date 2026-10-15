# Build stage
FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# Both binaries ship in one image: the installation gateway (default
# entrypoint) and the central gateway (HOR-661), which ECS runs as
# `central migrate up && exec central serve` through /bin/sh.
RUN for cmd in gateway central; do \
      CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-X github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version.version=${VERSION} -X github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version.commit=${COMMIT} -X github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version.date=${DATE}" \
        -o /out/$cmd ./cmd/$cmd || exit 1; \
    done

# Runtime stage
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/gateway /usr/local/bin/gateway
COPY --from=builder /out/central /usr/local/bin/central

EXPOSE 8080 8443 8081 9090

ENTRYPOINT ["gateway"]
CMD ["serve"]
