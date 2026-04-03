FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o auth-service ./services/auth

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/auth-service .

EXPOSE 8081

CMD ["./auth-service"]
