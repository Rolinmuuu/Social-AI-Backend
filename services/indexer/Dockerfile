FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o search-indexer ./services/indexer

FROM alpine:3.19
WORKDIR /app
# alpine has a CA bundle, needed for OpenAI (embeddings).
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/search-indexer .
CMD ["./search-indexer"]
