FROM golang:1.25-alpine AS build
WORKDIR /src/services/integration-service
COPY contracts/gen/go /src/contracts/gen/go
COPY packages/workload /src/packages/workload
COPY services/integration-service/go.mod services/integration-service/go.sum ./
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; go mod download
COPY services/integration-service .
RUN CGO_ENABLED=0 go build -o /integration-service ./cmd/server && CGO_ENABLED=0 go build -o /document-parser ./cmd/parser
FROM alpine:3.23 AS integration
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lawpilot
COPY --from=build /integration-service /usr/local/bin/
USER lawpilot
CMD ["integration-service"]
