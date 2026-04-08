FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o notification-worker ./services/notification/main.go

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /app/notification-worker .
CMD ["./notification-worker"]
