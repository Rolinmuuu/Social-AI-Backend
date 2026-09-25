FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o feed-worker ./services/feed

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /app/feed-worker .
CMD ["./feed-worker"]
