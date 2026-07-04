# syntax=docker/dockerfile:1.17

# Build stage for frontend
FROM node:22-alpine AS frontend-builder

WORKDIR /app/web

COPY web/package.json web/pnpm-lock.yaml* web/pnpm-workspace.yaml ./
RUN npm install -g pnpm@11.5.1 && pnpm install --frozen-lockfile

COPY web/ .
RUN pnpm run build

# Build stage for backend
FROM golang:1.26-alpine AS backend-builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o protect-homekit .

# ffmpeg is required at runtime to relay RTSPS -> HomeKit SRTP (video is
# copied, not transcoded, so the alpine ffmpeg with libopus is all we need).
FROM alpine:3.22

RUN apk add --no-cache ffmpeg tzdata && \
    adduser -D -H -u 1000 protect

COPY --from=backend-builder /app/protect-homekit /protect-homekit
COPY --from=frontend-builder /app/web/dist /web/dist

USER protect

ENTRYPOINT ["/protect-homekit", "/var/lib/protect-homekit/config.yaml"]
