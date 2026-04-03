FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o social-service ./services/social

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/social-service .

EXPOSE 8083

CMD ["./social-service"]
