# A throwaway Ubuntu server with systemd as PID 1, for scripts/e2e-deploy/test.sh.
# Everything deploy/relay.sh uses is real (apt, systemd, nginx, sshd)
# except certbot, which issues certificates from a local test CA instead of
# Let's Encrypt; the system trusts that CA. sqlite3 is left out so relay.sh installs it with apt.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl git iproute2 openssl \
      openssh-server python3 nginx certbot python3-certbot-dns-cloudflare \
 && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /etc/test-ca \
 && openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj /CN=seasoning-e2e-ca \
      -keyout /etc/test-ca/ca.key -out /etc/test-ca/ca.pem 2>/dev/null \
 && cp /etc/test-ca/ca.pem /usr/local/share/ca-certificates/seasoning-e2e-ca.crt \
 && update-ca-certificates >/dev/null \
 && install -d -m 0700 /root/.ssh \
 && systemctl enable ssh \
 && systemctl mask systemd-logind.service systemd-modules-load.service getty.target console-getty.service

COPY certbot /usr/local/bin/certbot
COPY releases.py /usr/local/bin/fake-releases

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
