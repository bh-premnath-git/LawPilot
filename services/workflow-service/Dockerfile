FROM golang:1.25-alpine AS build
WORKDIR /src/services/workflow-service
COPY contracts/gen/go /src/contracts/gen/go
COPY packages/workload /src/packages/workload
COPY services/workflow-service/go.mod services/workflow-service/go.sum ./
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; go mod download
COPY services/workflow-service .
RUN CGO_ENABLED=0 go build -o /execution-worker ./cmd/worker && CGO_ENABLED=0 go build -o /outbox-dispatcher ./cmd/dispatcher

FROM alpine:3.23
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lawpilot
WORKDIR /workspace
COPY --from=build /execution-worker /outbox-dispatcher /usr/local/bin/
COPY --chmod=0444 contracts/capabilities/registry.v1.json /workspace/contracts/capabilities/registry.v1.json
USER lawpilot
CMD ["execution-worker"]
