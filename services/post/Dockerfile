FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o post-service ./services/post

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/post-service .
# The slim image has no CA bundle; without it HTTPS calls (OpenAI, GCS) fail TLS verification.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

EXPOSE 8082

CMD ["./post-service"]
