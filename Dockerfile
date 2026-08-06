FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/situation-awareness-agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/situation-awareness-agent /usr/local/bin/situation-awareness-agent
ENTRYPOINT ["/usr/local/bin/situation-awareness-agent"]
