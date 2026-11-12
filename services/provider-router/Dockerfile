FROM golang:1.23-alpine AS build
WORKDIR /source
COPY services/provider-router/ ./
RUN CGO_ENABLED=0 go build -o /provider-router .
FROM alpine:3.20
COPY --from=build /provider-router /provider-router
USER 65532:65532
EXPOSE 8082
ENTRYPOINT ["/provider-router"]
