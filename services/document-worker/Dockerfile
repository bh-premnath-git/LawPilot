FROM rust:1.90.0-bookworm AS rust-build
WORKDIR /src
COPY services/document-worker/Cargo.toml services/document-worker/Cargo.lock ./
COPY services/document-worker/src ./src
RUN cargo build --release --locked
FROM golang:1.25-bookworm AS go-build
WORKDIR /src/services/integration-service
COPY contracts/gen/go /src/contracts/gen/go
COPY packages/workload /src/packages/workload
COPY services/integration-service/go.mod services/integration-service/go.sum ./
RUN go mod download
COPY services/integration-service .
RUN CGO_ENABLED=0 go build -o /document-parser ./cmd/parser
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils ca-certificates && rm -rf /var/lib/apt/lists/* && useradd --uid 10001 --no-create-home parser
COPY --from=rust-build /src/target/release/lawpilot-document-worker /usr/local/bin/lawpilot-document-worker
COPY --from=go-build /document-parser /usr/local/bin/document-parser
USER parser
ENV DOCUMENT_PROCESSOR_BIN=/usr/local/bin/lawpilot-document-worker
CMD ["document-parser"]
