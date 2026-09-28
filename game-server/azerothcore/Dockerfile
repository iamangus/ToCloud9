FROM ubuntu:22.04 AS build-sidecar

ENV DEBIAN_FRONTEND=noninteractive

WORKDIR /build

# Install build dependencies for libsidecar-cpp
RUN apt-get update && \
    apt-get install -y \
        git \
        cmake \
        make \
        gcc \
        g++ \
        build-essential \
        libssl-dev \
        zlib1g-dev \
        ca-certificates \
        pkg-config && \
    rm -rf /var/lib/apt/lists/*

# Copy source files (proto must be at /api/proto for CMake path resolution)
COPY api/proto /api/proto
COPY game-server/libsidecar-cpp /build/libsidecar-cpp

# Build libsidecar-cpp (clean tree — never reuse a host CMake cache)
WORKDIR /build/libsidecar-cpp
RUN rm -rf build && mkdir build && cd build && \
    cmake .. \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_TESTS=OFF \
        -DUSE_SYSTEM_GRPC=OFF \
        -DUSE_SYSTEM_PROTOBUF=OFF && \
    cmake --build . --config Release -j $(nproc)

FROM ubuntu:22.04 AS build-core

ENV DEBIAN_FRONTEND=noninteractive

# runs from project directory
WORKDIR /

RUN apt-get update || true && \
    apt-get install -y --reinstall ubuntu-keyring && \
    apt-get update

RUN apt-get install -yq git clang cmake make gcc g++ libmysqlclient-dev libssl-dev libbz2-dev libreadline-dev libncurses-dev libboost-all-dev mysql-server p7zip
RUN update-alternatives --install /usr/bin/cc cc /usr/bin/clang 100
RUN update-alternatives --install /usr/bin/c++ c++ /usr/bin/clang 100

ARG AZEROTHCORE_REPOSITORY=https://github.com/iamangus/azerothcore-wotlk.git
ARG AZEROTHCORE_REF=7f12e89ee5f467a50e62eba1d525eac7dc953d03
ARG MOD_PLAYERBOTS_REPOSITORY=https://github.com/iamangus/mod-playerbots.git
ARG MOD_PLAYERBOTS_REF=d30cb6659a8ba7090f7978947195c0de7e664f53

RUN git init /repo && \
    git -C /repo remote add origin "${AZEROTHCORE_REPOSITORY}" && \
    git -C /repo fetch --depth 1 origin "${AZEROTHCORE_REF}" && \
    git -C /repo checkout FETCH_HEAD

# Use the Playerbot-compatible AzerothCore fork. Its Playerbot branch already
# contains ToCloud9's TC9Sidecar hooks; the module is built into this same tree.
RUN mkdir -p /repo/modules/mod-playerbots && \
    git -C /repo/modules/mod-playerbots init && \
    git -C /repo/modules/mod-playerbots remote add origin "${MOD_PLAYERBOTS_REPOSITORY}" && \
    git -C /repo/modules/mod-playerbots fetch --depth 1 origin "${MOD_PLAYERBOTS_REF}" && \
    git -C /repo/modules/mod-playerbots checkout FETCH_HEAD

# Shared library (unversioned name; ABI is in tc9_version.h / TC9GetVersion)
COPY --from=build-sidecar /build/libsidecar-cpp/build/libsidecar.so ./repo/deps/libsidecar/libsidecar.so

# Public C API headers + generated version header (same build as the .so)
COPY --from=build-sidecar /build/libsidecar-cpp/include/libsidecar.h ./repo/deps/libsidecar/include/libsidecar.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/events-guild.h ./repo/deps/libsidecar/include/events-guild.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/events-group.h ./repo/deps/libsidecar/include/events-group.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/events-servers-registry.h ./repo/deps/libsidecar/include/events-servers-registry.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/player-items-api.h ./repo/deps/libsidecar/include/player-items-api.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/player-money-api.h ./repo/deps/libsidecar/include/player-money-api.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/player-interactions-api.h ./repo/deps/libsidecar/include/player-interactions-api.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/battleground-api.h ./repo/deps/libsidecar/include/battleground-api.h
COPY --from=build-sidecar /build/libsidecar-cpp/include/monitoring.h ./repo/deps/libsidecar/include/monitoring.h
COPY --from=build-sidecar /build/libsidecar-cpp/build/generated/include/tc9_version.h ./repo/deps/libsidecar/include/tc9_version.h

WORKDIR /repo

RUN cd bin && \
    cmake .. -DUSE_REAL_LIBSIDECAR=ON \
        -DBoost_USE_STATIC_LIBS=ON \
        -DWITH_WARNINGS=1 \
        -DWITH_COREDEBUG=0 \
        -DUSE_COREPCH=1 \
        -DUSE_SCRIPTPCH=1 \
        -DTOOLS_BUILD=db-only \
        -DSCRIPTS=static \
        -DSERVERS=1 \
        -DCMAKE_BUILD_TYPE=RelWithDebInfo \
        -DCMAKE_INSTALL_PREFIX=/repo/bin \
        -DBUILD_TESTING=0

RUN cp deps/libsidecar/libsidecar.so /usr/lib/libsidecar.so

WORKDIR /repo/bin

RUN cmake --build . --config "RelWithDebInfo" -j $(($(nproc) + 1))
RUN cmake --install . --config "RelWithDebInfo"

FROM ubuntu:22.04 AS db-import

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      libmysqlclient21 libreadline8 \
      gettext-base default-mysql-client && \
    rm -rf /var/lib/apt/lists/*


COPY --from=build-sidecar /build/libsidecar-cpp/build/libsidecar.so /usr/lib/libsidecar.so

# The paths are hardcoded in the binary
COPY --from=build-core /repo/data /repo/data
COPY --from=build-core /repo/bin/bin/dbimport /repo/bin/dbimport
COPY --from=build-core /repo/bin/etc/dbimport.conf.dist /repo/bin/etc/dbimport.conf

COPY sql/auth/mysql/ /repo/data/sql/custom/db_auth
COPY sql/characters/mysql/*.up.sql /repo/data/sql/custom/db_characters

WORKDIR /repo/bin

ENTRYPOINT ["./dbimport"]

FROM ubuntu:22.04 AS gameserver

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      libmysqlclient21 libreadline8 \
      gettext-base default-mysql-client && \
    rm -rf /var/lib/apt/lists/*

COPY --from=build-sidecar /build/libsidecar-cpp/build/libsidecar.so /usr/lib/libsidecar.so

COPY --from=build-core /repo/bin/etc/worldserver.conf.dist /repo/bin/etc/worldserver.conf
COPY --from=build-core /repo/bin/bin/worldserver /repo/bin/worldserver
# PlayerbotDatabaseScript loads this SQL tree at worldserver startup.
COPY --from=build-core /repo/modules/mod-playerbots/data/sql/playerbots /repo/modules/mod-playerbots/data/sql/playerbots

WORKDIR /repo/bin

ENTRYPOINT ["./worldserver"]
