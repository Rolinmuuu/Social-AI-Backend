FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o social-service ./services/social

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/social-service .
# The slim image has no CA bundle; without it HTTPS calls (OpenAI, GCS) fail TLS verification.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

EXPOSE 8083

CMD ["./social-service"]
