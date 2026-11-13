# WI 0.1 route 1: a privileged systemd container whose Pi-hole v5 runtime
# (/etc/.pihole, /opt/pihole, /usr/local/bin/pihole, /usr/bin/pihole-FTL,
# /etc/pihole, /etc/dnsmasq.d) is the one pihole/pihole:2024.07.0 ships:
# core v5.18.3, web v5.21, FTL v5.25.2 (V-18, ASM-13). Pinned by digest.
# spike.sh builds it; nothing is bind-mounted.
FROM pihole/pihole:2024.07.0@sha256:0def896a596e8d45780b6359dbf82fc8c75ef05b97e095452e67a0a4ccc95377

# How pihole-FTL is started under systemd:
#   sysv   (WI 0.1 default) the v5 SysV script as /etc/init.d/pihole-FTL,
#          which systemd-sysv-generator wraps in a generated unit;
#   native the unit v5.18.x basic-install.sh installs when /run/systemd/system
#          exists (installScripts: pihole-FTL.systemd as
#          /etc/systemd/system/pihole-FTL.service, init.d script removed).
ARG INIT=sysv

ENV container=docker DEBIAN_FRONTEND=noninteractive

# systemd, dbus and dnsutils; the s6 `service` shim in /usr/local/bin goes,
# so `pihole restartdns` -> `service pihole-FTL restart` reaches Debian's
# /usr/sbin/service and through it systemd. Packages come from the
# snapshot.debian.org date the image itself was built from (its own
# sources.list names it): bullseye has left deb.debian.org's security pool,
# and the snapshot keeps the new packages in step with the image's libraries.
ARG SNAPSHOT=20240612T000000Z
RUN printf 'deb http://snapshot.debian.org/archive/debian/%s bullseye main\ndeb http://snapshot.debian.org/archive/debian-security/%s bullseye-security main\ndeb http://snapshot.debian.org/archive/debian/%s bullseye-updates main\n' \
      "$SNAPSHOT" "$SNAPSHOT" "$SNAPSHOT" >/etc/apt/sources.list \
 && apt-get -o Acquire::Check-Valid-Until=false update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dnsutils \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /usr/local/bin/service \
 && { grep -qx 'conf-dir=/etc/dnsmasq.d' /etc/dnsmasq.conf || echo 'conf-dir=/etc/dnsmasq.d' >>/etc/dnsmasq.conf; }

RUN set -e; \
    case "$INIT" in \
    sysv) \
      install -T -m 0755 /etc/.pihole/advanced/Templates/pihole-FTL.service /etc/init.d/pihole-FTL; \
      update-rc.d pihole-FTL defaults ;; \
    native) \
      install -T -m 0644 /etc/.pihole/advanced/Templates/pihole-FTL.systemd /etc/systemd/system/pihole-FTL.service; \
      rm -f /etc/init.d/pihole-FTL; \
      systemctl enable pihole-FTL.service ;; \
    *) echo "unknown INIT=$INIT" >&2; exit 1 ;; \
    esac; \
    echo "$INIT" >/etc/pihole-spike-init

# The image's s6 entrypoint and health check belong to its own init.
ENTRYPOINT []
HEALTHCHECK NONE
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
