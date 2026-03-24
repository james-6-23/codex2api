FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /codex2api .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /codex2api /usr/local/bin/codex2api

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/codex2api"]
