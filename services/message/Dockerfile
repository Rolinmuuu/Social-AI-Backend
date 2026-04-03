FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o message-service ./services/message

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/message-service .

EXPOSE 8084

CMD ["./message-service"]
