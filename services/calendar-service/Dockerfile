FROM golang:1.25-alpine AS build
WORKDIR /src/services/calendar-service
COPY contracts/gen/go /src/contracts/gen/go
COPY packages/workload /src/packages/workload
COPY services/calendar-service/go.mod services/calendar-service/go.sum ./
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; go mod download
COPY services/calendar-service .
RUN CGO_ENABLED=0 go build -o /calendar-service ./cmd/server
FROM alpine:3.23
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lawpilot
COPY --from=build /calendar-service /usr/local/bin/calendar-service
USER lawpilot
CMD ["calendar-service"]
