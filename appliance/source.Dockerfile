# Isolated host tools for the experimental OpenWrt source build.
FROM debian:bookworm-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    build-essential ca-certificates clang file flex bison g++ gawk gcc-multilib \
    gettext git libncurses-dev libssl-dev python3 python3-setuptools python3-dev \
    rsync swig unzip zlib1g-dev wget curl xz-utils zstd patch perl time \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 1000 builder
USER builder
WORKDIR /build
ENV LANG=C.UTF-8
