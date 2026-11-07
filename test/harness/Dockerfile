# T1 harness image: a privileged systemd container (architecture.md,
# Verification Architecture). BASE tracks the current release unpinned
# (ASM-9); run.sh passes it. Nothing is bind-mounted: binaries, the unit and
# lib.sh arrive by docker cp.
ARG BASE=ubuntu:latest
FROM ${BASE}

ENV container=docker DEBIAN_FRONTEND=noninteractive

# docker-compose-v2 is an Ubuntu package; on a base without it the Docker
# tests skip (NFR-6 runs at BASE=debian:stable).
RUN apt-get update \
 && compose="$(apt-cache show docker-compose-v2 >/dev/null 2>&1 && echo docker-compose-v2 || true)" \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus needrestart docker.io ${compose} dpkg-dev ca-certificates \
      iproute2 procps util-linux git \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

# systemd's documented container stop signal.
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
