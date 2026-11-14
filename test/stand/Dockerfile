# The stand host: Ubuntu with systemd as PID 1, Docker 29 with the containerd image store, Go to
# build clowdrd from the working tree, and node LTS with corepack for npm and yarn 4.
FROM ubuntu:24.04
ARG GO_VERSION
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd systemd-sysv dbus ca-certificates curl jq openssl iptables iproute2 gnupg git zstd psmisc \
    && curl -fsSL https://get.docker.com | sh \
    && mkdir -p /etc/docker && echo '{"features": {"containerd-snapshotter": true}}' >/etc/docker/daemon.json \
    && systemctl mask systemd-resolved.service getty.target console-getty.service
# Node 25+ no longer ships corepack: it comes from npm.
RUN curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g corepack && corepack enable \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-$(dpkg --print-architecture).tar.gz" | tar -xz -C /usr/local \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
