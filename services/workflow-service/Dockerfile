FROM golang:1.25-alpine AS build
WORKDIR /src
COPY services/workflow-service/go.mod services/workflow-service/go.sum ./
RUN go mod download
COPY services/workflow-service .
RUN CGO_ENABLED=0 go build -o /execution-worker ./cmd/worker && CGO_ENABLED=0 go build -o /outbox-dispatcher ./cmd/dispatcher

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lawpilot
WORKDIR /workspace
COPY --from=build /execution-worker /outbox-dispatcher /usr/local/bin/
COPY --chmod=0444 contracts/capabilities/registry.v1.json /workspace/contracts/capabilities/registry.v1.json
USER lawpilot
CMD ["execution-worker"]
