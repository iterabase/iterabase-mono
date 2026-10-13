# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder
WORKDIR /src
COPY main.go smtp.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/e2e-runtime ./main.go ./smtp.go

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
RUN apk add --no-cache ca-certificates git openssh
COPY --from=builder /out/e2e-runtime /usr/local/bin/e2e-runtime
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/e2e-runtime"]
