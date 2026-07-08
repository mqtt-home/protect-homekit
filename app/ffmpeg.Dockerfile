# Base image carrying an ffmpeg built with libfdk_aac (for AAC-ELD, HomeKit's
# native camera audio codec). Alpine's stock ffmpeg omits libfdk_aac (non-free
# license), so ffmpeg is compiled against Alpine's fdk-aac/x264/opus dev
# packages. Built rarely (only when the ffmpeg version changes) and published as
# pharndt/protect-ffmpeg, so the per-release app build just FROMs it instead of
# recompiling ffmpeg every time — an in-line compile blows GoReleaser's 1h
# timeout under QEMU. See .github/workflows/build-ffmpeg.yml.
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

# Shared libraries the compiled ffmpeg links against (same Alpine version as the
# builder, so the ABI matches). tzdata for local timestamps.
RUN apk add --no-cache \
    x264-libs fdk-aac opus libssl3 libcrypto3 libgcc zlib tzdata

COPY --from=ffmpeg-builder /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-builder /usr/local/bin/ffprobe /usr/local/bin/ffprobe

# Fail the build (not at app runtime) if the binary can't load or lacks AAC-ELD.
RUN ffmpeg -hide_banner -encoders | grep -q libfdk_aac
