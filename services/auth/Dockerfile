FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o auth-service ./services/auth

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/auth-service .
# The slim image has no CA bundle; without it HTTPS calls (OpenAI, GCS) fail TLS verification.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

EXPOSE 8081

CMD ["./auth-service"]
