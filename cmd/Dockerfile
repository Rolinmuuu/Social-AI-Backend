# One image for the operational one-shot commands:
#   migrate  - apply SQL migrations (runs before the services in docker compose)
#   reindex  - rebuild the post search index from PostgreSQL
#   backfill - one-time copy of the legacy Elasticsearch data into PostgreSQL
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -o /out/reindex ./cmd/reindex && \
    CGO_ENABLED=0 go build -o /out/backfill ./cmd/backfill

FROM alpine:3.19
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ ./
CMD ["./migrate"]
