FROM debian:13 AS build

RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
      build-essential cmake meson ninja-build pkg-config python3-pyelftools \
      libnuma-dev wget xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# --- DPDK を static + 対象CPU向けにビルド ---
ARG DPDK_VER=24.11
WORKDIR /src
RUN wget -q https://fast.dpdk.org/rel/dpdk-${DPDK_VER}.tar.xz \
    && mkdir -p dpdk \
    && tar xf dpdk-${DPDK_VER}.tar.xz -C dpdk --strip-components=1 \
    && rm dpdk-${DPDK_VER}.tar.xz

WORKDIR /src/dpdk
RUN meson setup build \
      --prefix=/opt/dpdk \
      --default-library=static \
      -Dcpu_instruction_set=x86-64-v3 \
      -Denable_drivers=net/ixgbe \
      -Dtests=false -Denable_kmods=false \
    && ninja -C build \
    && ninja -C build install

# --- アプリを static リンクでビルド ---
ENV PKG_CONFIG_PATH=/opt/dpdk/lib/x86_64-linux-gnu/pkgconfig
WORKDIR /app
COPY . .
RUN python3 build.py

# --- 成果物だけ取り出す用の空ステージ ---
FROM scratch AS artifact
COPY --from=build / /dpdk_app
