FROM golang:1.25-alpine AS build
WORKDIR /src/services/context-engine
COPY contracts/gen/go /src/contracts/gen/go
COPY packages/workload /src/packages/workload
COPY services/context-engine/go.mod services/context-engine/go.sum ./
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; go mod download
COPY services/context-engine .
RUN CGO_ENABLED=0 go build -o /context-engine ./cmd/server && CGO_ENABLED=0 go build -o /domain-events ./cmd/events
FROM alpine:3.23
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lawpilot
COPY --from=build /context-engine /domain-events /usr/local/bin/
COPY --chmod=0444 contracts/events/registry.v1.json /workspace/contracts/events/registry.v1.json
WORKDIR /workspace
USER lawpilot
CMD ["context-engine"]
