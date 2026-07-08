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

# ffmpeg with libfdk_aac: relays RTSPS -> HomeKit SRTP (video copied) and lets
# cameras stream AAC-ELD, which iOS wants for Secure Video. Alpine's stock
# ffmpeg omits libfdk_aac (non-free), so compile ffmpeg against Alpine's
# fdk-aac/x264/opus dev packages; only ffmpeg is built from source.
FROM alpine:3.22 AS ffmpeg-builder

ARG FFMPEG_VERSION=n7.1

RUN apk add --no-cache \
    build-base pkgconf nasm yasm git \
    x264-dev fdk-aac-dev opus-dev openssl-dev zlib-dev

RUN git clone --depth 1 --branch "${FFMPEG_VERSION}" https://github.com/FFmpeg/FFmpeg.git /ffmpeg-src \
    && cd /ffmpeg-src \
    && ./configure \
        --prefix=/usr/local \
        --enable-gpl --enable-nonfree --enable-version3 \
        --enable-libx264 --enable-libfdk-aac --enable-libopus \
        --enable-openssl \
        --disable-doc --disable-ffplay \
    && make -j"$(nproc)" \
    && make install \
    && /usr/local/bin/ffmpeg -hide_banner -encoders | grep -q libfdk_aac

FROM alpine:3.22

RUN apk add --no-cache \
    x264-libs fdk-aac opus libssl3 libcrypto3 libgcc zlib tzdata \
    && adduser -D -H -u 1000 protect

COPY --from=ffmpeg-builder /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-builder /usr/local/bin/ffprobe /usr/local/bin/ffprobe
RUN ffmpeg -hide_banner -encoders | grep -q libfdk_aac

COPY --from=backend-builder /app/protect-homekit /protect-homekit
COPY --from=frontend-builder /app/web/dist /web/dist

USER protect

ENTRYPOINT ["/protect-homekit", "/var/lib/protect-homekit/config.yaml"]
