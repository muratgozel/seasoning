#!/usr/bin/env bash
# End-to-end test of deploy/relay.sh on a throwaway Ubuntu server: a Docker
# container with systemd as PID 1. apt, systemd, nginx, sshd, the installer
# and the relay are real; GitHub releases are served from bin/e2e-deploy and
# certbot is a stand-in that signs with a test CA (see the Dockerfile).
#
#   make e2e-deploy
#   E2E_KEEP=1 make e2e-deploy     keep the container afterwards
#
# Releases: v0.1.0 and v0.1.1 are real builds; v0.1.2 runs `version` but
# can't serve, to exercise the automatic rollback.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME=seasoning-e2e-deploy
W="$ROOT/bin/e2e-deploy"
REL="$W/releases"
DOMAIN=relay.test
CF_TOKEN=e2e-cloudflare-token-0123456789
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

step() {
	STEP_START=$(date +%s)
	printf '%-64s ' "$1"
}
ok() { echo "ok ($(($(date +%s) - STEP_START))s)"; }
fail() {
	echo FAIL
	printf '  %s\n' "$1" >&2
	echo "--- relay log ---" >&2
	docker exec "$NAME" journalctl -u seasoning-relay -n 30 --no-pager >&2 || true
	KEEP=1
	exit 1
}

KEEP="${E2E_KEEP:-0}"
cleanup() {
	if [ "$KEEP" = 1 ]; then
		echo "e2e-deploy: container $NAME kept; inspect: docker exec -it $NAME bash"
	else
		docker rm -f "$NAME" >/dev/null 2>&1 || true
	fi
}

# x runs a command on the server; relay runs relay.sh there as root.
x() { docker exec "$NAME" "$@"; }
relay() {
	docker exec -e SEASONING_INSTALLER_URL=http://127.0.0.1:8000/install.sh \
		-e SEASONING_RELEASES_BASE=http://127.0.0.1:8000 \
		"$NAME" bash /src/relay.sh "$@" 2>&1
}
serving() { x curl -fsS --max-time 3 "http://127.0.0.1:${1:-8090}/healthz" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }
projects() { x runuser -u seasoning -- sqlite3 /var/lib/seasoning/relay.db 'SELECT count(*) FROM projects'; }
token() { x sed -n 's/^SEASONING_RELAY_TOKEN=//p' /etc/seasoning/relay.env; }
# init_project N TOKEN: a developer creates project N through nginx and TLS.
init_project() {
	docker exec -e HOME=/root/dev -e "SEASONING_RELAY_TOKEN=$2" "$NAME" bash -c "
		set -e
		[ -d /root/dev/.config/seasoning ] || seasoning setup --handle ops >/dev/null
		mkdir -p /root/dev/repo$1 && cd /root/dev/repo$1
		[ -d .git ] || { git init -q && git remote add origin https://github.com/example/deploy-e2e-$1.git; }
		seasoning init --relay https://$DOMAIN" 2>&1
}

# --- fixtures ----------------------------------------------------------------

release() { # VERSION BINARY
	local dir="$REL/releases/download/v$1" asset stage
	asset="seasoning_$1_linux_${arch}.tar.gz"
	stage="$W/stage-$1"
	rm -rf "$dir" "$stage" && mkdir -p "$dir" "$stage"
	cp "$2" "$stage/seasoning" && cp "$ROOT/README.md" "$ROOT/LICENSE" "$stage/"
	chmod 755 "$stage/seasoning"
	tar -C "$stage" -czf "$dir/$asset" seasoning README.md LICENSE
	(cd "$dir" && shasum -a 256 "$asset" >checksums.txt)
}

arch="$(go env GOARCH)"
echo "e2e-deploy: building releases for linux/$arch"
rm -rf "$W" && mkdir -p "$REL"
for v in 0.1.0 0.1.1; do
	(cd "$ROOT" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X github.com/muratgozel/seasoning/internal/version.Version=$v" \
		-o "$W/seasoning-$v" ./cmd/seasoning)
	release "$v" "$W/seasoning-$v"
done
cat >"$W/seasoning-0.1.2" <<'EOF'
#!/bin/sh
[ "$1" = version ] && echo "seasoning 0.1.2" && exit 0
echo "broken build" >&2
exit 1
EOF
release 0.1.2 "$W/seasoning-0.1.2"
cp "$ROOT/scripts/install.sh" "$REL/install.sh"
echo v0.1.0 >"$REL/LATEST"
ssh-keygen -q -t ed25519 -N '' -C e2e-deploy -f "$W/id_ed25519"

echo "e2e-deploy: starting the server"
docker build -q -t seasoning-e2e:deploy "$ROOT/scripts/e2e-deploy" >/dev/null
docker rm -f "$NAME" >/dev/null 2>&1 || true
trap cleanup EXIT
docker run -d --name "$NAME" --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	--tmpfs /run --tmpfs /run/lock --add-host "$DOMAIN:127.0.0.1" -p 127.0.0.1::22 \
	-v "$ROOT/deploy:/src:ro" -v "$REL:/srv/releases:ro" seasoning-e2e:deploy >/dev/null
state=""
for _ in $(seq 1 120); do
	state=$(x systemctl is-system-running 2>/dev/null || true)
	case "$state" in running | degraded) break ;; esac
	sleep 0.5
done
[ "$state" = running ] || fail "systemd in the container is '$state': $(x systemctl --failed --no-legend 2>&1)"
docker exec -d "$NAME" python3 /usr/local/bin/fake-releases /srv/releases
docker exec -i "$NAME" sh -c 'cat >/root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys' <"$W/id_ed25519.pub"
for _ in $(seq 1 50); do
	if x curl -fsS -o /dev/null http://127.0.0.1:8000/LATEST 2>/dev/null; then break; fi
	sleep 0.2
done

# --- scenario ----------------------------------------------------------------

step "install on a fresh server"
out=$(relay install) || fail "install failed: $out"
[ "$(serving)" = 0.1.0 ] || fail "not serving 0.1.0 after install: $out"
[ "$(x readlink /usr/local/bin/seasoning)" = /opt/seasoning/versions/0.1.0 ] || fail "binary link is wrong"
[ "$(x stat -c '%a %U' /etc/seasoning/relay.env)" = "600 root" ] || fail "relay.env must be 0600 root"
[ "$(token | wc -c | tr -d ' ')" -ge 33 ] || fail "token too short"
x dpkg-query -W sqlite3 >/dev/null || fail "sqlite3 wasn't installed with apt"
x systemctl is-enabled --quiet seasoning-relay || fail "service not enabled"
case "$out" in *"next: relay.sh tls"*) ;; *) fail "install didn't point to tls: $out" ;; esac
ok

step "install again changes nothing"
tok=$(token)
pid=$(x systemctl show -p MainPID --value seasoning-relay)
out=$(relay install) || fail "second install failed: $out"
[ "$(token)" = "$tok" ] || fail "install replaced the token"
[ "$(x systemctl show -p MainPID --value seasoning-relay)" = "$pid" ] || fail "install restarted a healthy relay"
case "$out" in *"unchanged: /etc/systemd/system/seasoning-relay.service"*) ;; *) fail "unit rewritten: $out" ;; esac
ok

step "tls issues a certificate and serves https through nginx"
out=$(docker exec -e CLOUDFLARE_API_TOKEN="$CF_TOKEN" "$NAME" \
	bash /src/relay.sh tls --domain "$DOMAIN" --email ops@relay.test 2>&1) || fail "tls failed: $out"
x grep -q -- "--dns-cloudflare-propagation-seconds 30 -d $DOMAIN --email ops@relay.test" /var/log/fake-certbot.log || fail "certbot args: $(x cat /var/log/fake-certbot.log)"
x grep -qx "dns_cloudflare_api_token = $CF_TOKEN" /etc/letsencrypt/cloudflare/credentials.ini || fail "credentials not stored"
x curl -fsS "https://$DOMAIN/healthz" | grep -q '"version":"0.1.0"' || fail "https health failed"
[ "$(x curl -s -o /dev/null -w '%{http_code}' "http://$DOMAIN/healthz")" = 301 ] || fail "http doesn't redirect"
[ "$(x curl -s -o /dev/null -w '%{http_code}' "https://$DOMAIN/v1/nope")" = 404 ] || fail "unknown paths aren't 404"
[ "$(x curl -s -o /dev/null -w '%{http_code}' "https://$DOMAIN/")" = 404 ] || fail "/ isn't 404"
ok

step "tls again keeps the certificate and site"
out=$(relay tls) || fail "second tls failed: $out"
[ "$(x sh -c 'wc -l </var/log/fake-certbot.log')" -eq 1 ] || fail "certbot ran again"
case "$out" in *"unchanged: /etc/nginx/sites-available/seasoning.conf"*) ;; *) fail "site rewritten: $out" ;; esac
ok

step "a developer creates a project with the relay token"
out=$(init_project 1 "$(token)") || fail "seasoning init failed: $out"
case "$out" in *"https://$DOMAIN/join#k="*) ;; *) fail "no invite in: $out" ;; esac
[ "$(projects)" = 1 ] || fail "project count $(projects)"
ok

step "status reports a healthy relay and certificate"
out=$(relay status) || fail "status failed: $out"
for want in "version 0.1.0" "ok: https://$DOMAIN" "certificate" "(89 days)"; do
	case "$out" in *"$want"*) ;; *) fail "status lacks '$want': $out" ;; esac
done
ok

step "backup writes a checked, root-only copy"
out=$(relay backup) || fail "backup failed: $out"
backup1=$(x sh -c 'ls -1t /var/backups/seasoning/relay-*-manual.db | head -n 1')
[ "$(x stat -c '%a %U' "$backup1")" = "600 root" ] || fail "backup must be 0600 root"
[ "$(x sqlite3 -readonly "$backup1" 'SELECT count(*) FROM projects')" = 1 ] || fail "backup lacks the project"
[ -z "$(x find /var/lib/seasoning -not -user seasoning)" ] || fail "root-owned files in the data dir: $(x find /var/lib/seasoning -not -user seasoning)"
ok

step "update to v0.1.1 backs up first"
out=$(relay update --version v0.1.1) || fail "update failed: $out"
[ "$(serving)" = 0.1.1 ] || fail "not serving 0.1.1: $out"
x sh -c 'ls /var/backups/seasoning/relay-*-before-0.1.1.db' >/dev/null || fail "no pre-update backup"
[ "$(projects)" = 1 ] || fail "project lost in the update"
ok

step "a broken release rolls back automatically"
if out=$(relay update --version v0.1.2); then fail "update to the broken release succeeded: $out"; fi
case "$out" in *"back on 0.1.1"*) ;; *) fail "no rollback message: $out" ;; esac
[ "$(serving)" = 0.1.1 ] || fail "not serving 0.1.1 after the rollback"
x test ! -e /opt/seasoning/versions/0.1.2 || fail "the broken version was kept"
[ "$(projects)" = 1 ] || fail "project lost in the rollback"
ok

step "rollback switches to the previous version"
out=$(relay rollback) || fail "rollback failed: $out"
[ "$(serving)" = 0.1.0 ] || fail "not serving 0.1.0: $out"
ok

step "update without a version installs the latest release"
echo v0.1.1 >"$REL/LATEST"
out=$(relay update) || fail "update failed: $out"
[ "$(serving)" = 0.1.1 ] || fail "not serving 0.1.1: $out"
out=$(relay update) || fail "second update failed: $out"
case "$out" in *"already running the latest release, 0.1.1"*) ;; *) fail "not a no-op: $out" ;; esac
ok

step "token rotation locks out the old token only"
old=$(token)
out=$(relay token --rotate --yes) || fail "rotation failed: $out"
new=$(token)
[ "$new" != "$old" ] && [ -n "$new" ] || fail "token unchanged"
case "$out" in *"$new"*) ;; *) fail "new token not printed" ;; esac
if out=$(init_project 2 "$old"); then fail "the old token still creates projects"; fi
out=$(init_project 2 "$new") || fail "the new token was refused: $out"
[ "$(projects)" = 2 ] || fail "project count $(projects)"
ok

step "restore brings back a backup"
out=$(relay restore "$backup1" --yes) || fail "restore failed: $out"
[ "$(projects)" = 1 ] || fail "restore didn't take effect: $(projects) projects"
[ "$(serving)" = 0.1.1 ] || fail "not healthy after restore"
x sh -c 'ls /var/lib/seasoning/relay.db.replaced-*' >/dev/null || fail "previous database not kept"
[ -z "$(x find /var/lib/seasoning -not -user seasoning)" ] || fail "root-owned files in the data dir"
ok

step "update adopts a hand-installed binary"
x sh -c 'cp /opt/seasoning/versions/0.1.0 /usr/local/bin/seasoning.new && mv -f /usr/local/bin/seasoning.new /usr/local/bin/seasoning && rm -rf /opt/seasoning/versions && systemctl reset-failed seasoning-relay; systemctl restart seasoning-relay'
for _ in $(seq 1 40); do
	if [ "$(serving)" = 0.1.0 ]; then break; fi
	sleep 0.25
done
[ "$(serving)" = 0.1.0 ] || fail "hand-installed 0.1.0 didn't start"
out=$(relay update) || fail "update failed: $out"
[ "$(serving)" = 0.1.1 ] || fail "not serving 0.1.1: $out"
x test -f /opt/seasoning/versions/0.1.0 || fail "the hand-installed binary wasn't kept for rollback"
ok

step "--ssh runs commands on the server from macOS bash"
port=$(docker port "$NAME" 22/tcp | head -n 1 | sed 's/.*://')
export SEASONING_SSH="ssh -p $port -i $W/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes"
out=$(/bin/bash "$ROOT/deploy/relay.sh" --ssh root@127.0.0.1 status 2>&1) || fail "remote status failed: $out"
case "$out" in *"version 0.1.1"*) ;; *) fail "remote status: $out" ;; esac
[ "$(/bin/bash "$ROOT/deploy/relay.sh" --ssh root@127.0.0.1 token 2>/dev/null | tr -d '\r')" = "$(token)" ] || fail "remote token differs"
if out=$(/bin/bash "$ROOT/deploy/relay.sh" --ssh root@127.0.0.1 render nginx --domain "x'; touch /tmp/pwned; '" 2>&1); then fail "bad domain accepted"; fi
x test ! -e /tmp/pwned || fail "arguments weren't quoted for the remote shell"
ok

step "logs shows the relay's journal"
out=$(relay logs -n 5 --no-pager) || fail "logs failed: $out"
[ -n "$out" ] || fail "no log lines"
ok

step "a port change moves nginx with the relay"
out=$(relay install --port 8091) || fail "install --port failed: $out"
[ "$(serving 8091)" = 0.1.1 ] || fail "not serving on 8091: $out"
x grep -q 'proxy_pass http://127.0.0.1:8091;' /etc/nginx/sites-available/seasoning.conf || fail "nginx still proxies the old port"
x curl -fsS "https://$DOMAIN/healthz" >/dev/null || fail "https broken after the port change"
ok

step "uninstall keeps data; install picks it back up"
tok=$(token)
out=$(relay uninstall --yes) || fail "uninstall failed: $out"
x test ! -e /etc/systemd/system/seasoning-relay.service || fail "unit left behind"
x test ! -e /etc/nginx/sites-enabled/seasoning.conf || fail "site left behind"
x test -f /var/lib/seasoning/relay.db || fail "database deleted without --purge"
out=$(relay install) || fail "reinstall failed: $out"
out=$(relay tls --domain "$DOMAIN") || fail "tls after reinstall failed: $out"
[ "$(token)" = "$tok" ] && [ "$(projects)" = 1 ] || fail "token or projects lost"
x curl -fsS "https://$DOMAIN/healthz" >/dev/null || fail "https broken after reinstall"
ok

step "uninstall --purge removes the relay's data and user"
out=$(relay uninstall --purge --yes) || fail "purge failed: $out"
for p in /var/lib/seasoning /etc/seasoning /var/backups/seasoning /opt/seasoning /usr/local/bin/seasoning; do
	x test ! -e "$p" || fail "$p left behind"
done
if x id -u seasoning >/dev/null 2>&1; then fail "seasoning user left behind"; fi
ok

echo "e2e-deploy: all checks passed"
