# syntax=docker/dockerfile:1.17

FROM golang:1.26-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o protect-homekit .

# ffmpeg is required at runtime to relay RTSPS -> HomeKit SRTP (video is
# copied, not transcoded, so the alpine ffmpeg with libopus is all we need).
FROM alpine:3.22

RUN apk add --no-cache ffmpeg tzdata && \
    adduser -D -H -u 1000 protect

COPY --from=builder /app/protect-homekit /protect-homekit

USER protect

ENTRYPOINT ["/protect-homekit", "/var/lib/protect-homekit/config.yaml"]
