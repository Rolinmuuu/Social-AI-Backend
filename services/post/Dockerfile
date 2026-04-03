FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o post-service ./services/post

FROM debian:bookworm-slim

WORKDIR /app

COPY --from=builder /app/post-service .

EXPOSE 8082

CMD ["./post-service"]
