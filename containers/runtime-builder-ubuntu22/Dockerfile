FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates cmake git libboost-program-options-dev libgmp-dev \
    libprocps-dev libssl-dev pkg-config && \
    rm -rf /var/lib/apt/lists/*

ENV PATH=/opt/go/bin:$PATH
WORKDIR /workspace
