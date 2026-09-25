FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o message-service ./services/message

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/message-service .
# The slim image has no CA bundle; without it HTTPS calls (OpenAI, GCS) fail TLS verification.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

EXPOSE 8084

CMD ["./message-service"]
