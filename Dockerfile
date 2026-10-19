FROM golang:1.26-alpine AS builder
RUN apk --no-cache add git ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app .

FROM scratch
COPY --from=builder /app /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
EXPOSE 8080
ENV PORT=8080 \
    LOG_LEVEL=info \
    GOGC=25
ENTRYPOINT ["/app"]
