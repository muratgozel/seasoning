#!/usr/bin/env bash
# Deploy and maintain a seasoning relay on a Debian or Ubuntu server with
# systemd: the relay service, nginx with a Let's Encrypt certificate from
# certbot's Cloudflare DNS challenge, updates with automatic rollback, and
# backups. Every command is idempotent.
#
# On the server:
#   curl -fsSLO https://raw.githubusercontent.com/muratgozel/seasoning/main/deploy/relay.sh
#   sudo bash relay.sh install
# From your machine, through SSH (the script copies itself; sudo may ask):
#   deploy/relay.sh --ssh myserver install
#
# `relay.sh help` lists the commands. Env:
#   CLOUDFLARE_API_TOKEN     tls: the token to store (otherwise asked once)
#   SEASONING_SSH            --ssh: the ssh command (default: ssh)
#   SEASONING_INSTALLER_URL  the installer to run (tests, mirrors)
#   SEASONING_RELEASES_BASE  passed to the installer (tests, mirrors)
#
# Run by root on the server; only help, render and --ssh run elsewhere. Those
# three must keep working in macOS's bash 3.2.
set -euo pipefail

SERVICE=seasoning-relay
UNIT=/etc/systemd/system/$SERVICE.service
BIN_DIR=/usr/local/bin
BIN=$BIN_DIR/seasoning
VERSIONS_DIR=/opt/seasoning/versions
ETC_DIR=/etc/seasoning
ENV_FILE=$ETC_DIR/relay.env
DATA_DIR=/var/lib/seasoning
DB=$DATA_DIR/relay.db
BACKUP_DIR=/var/backups/seasoning
BACKUP_KEEP=10
SITE=/etc/nginx/sites-available/seasoning.conf
SITE_LINK=/etc/nginx/sites-enabled/seasoning.conf
CF_DIR=/etc/letsencrypt/cloudflare
CF_INI=$CF_DIR/credentials.ini
DEFAULT_PORT=8090
INSTALLER_URL=${SEASONING_INSTALLER_URL:-https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh}

ORIG_ARGS=("$@")

say() { printf '%s\n' "$*"; }
step() { printf '==> %s\n' "$*"; }
warn() { printf 'relay.sh: warning: %s\n' "$*" >&2; }
die() {
	printf 'relay.sh: %s\n' "$*" >&2
	exit 1
}
utc_stamp() { date -u +%Y%m%dT%H%M%SZ; }

usage() {
	cat <<'EOF'
usage: relay.sh [--ssh HOST] COMMAND [options]

Set up
  install [--port N] [--version vX.Y.Z]
      Install or repair the relay: binary, system user, token, systemd unit.
      Keeps an existing token, database and binary (use update to upgrade).
  tls --domain HOST [--email ADDR] [--dns-wait SECONDS]
      nginx in front of the relay with a Let's Encrypt certificate from the
      Cloudflare DNS challenge. The email is needed for the first certificate.

Maintain
  status                       service, version, health, certificate, database
  update [--version vX.Y.Z]    back up, install, restart; rolls back on failure
  rollback [--version X.Y.Z]   switch to another installed version
  backup [--dir DIR]           consistent copy of the database (keeps 10)
  restore FILE [--yes]         replace the database with a backup
  token [--rotate] [--yes]     print the relay token (for seasoning init)
  logs [journalctl options]    the relay's log (e.g. logs -f)
  uninstall [--purge] [--yes]  remove the relay; --purge also deletes its data

Other
  render unit|nginx [--port N] [--domain HOST]   print what install/tls write
  help

--ssh HOST runs the command on HOST: the script copies itself there and
runs with sudo.
EOF
}

# --- templates ---------------------------------------------------------------
# deploy/systemd/seasoning-relay.service and deploy/nginx/seasoning.conf are
# these templates rendered with the defaults; CI fails when they differ.

render_unit() {
	local port=$1
	cat <<EOF
# The seasoning relay as a systemd service. deploy/relay.sh installs it
# (relay.sh install); change the template there.
[Unit]
Description=seasoning relay
Documentation=https://github.com/muratgozel/seasoning
After=network-online.target
Wants=network-online.target

[Service]
User=seasoning
Group=seasoning
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve --listen 127.0.0.1:$port --db $DB --trust-proxy
Restart=on-failure
RestartSec=2
TimeoutStopSec=15
StateDirectory=seasoning
StateDirectoryMode=0700

NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
EOF
}

render_nginx() {
	local domain=$1 port=$2
	sed -e "s/@DOMAIN@/$domain/g" -e "s/@PORT@/$port/g" <<'EOF'
# nginx in front of the seasoning relay. deploy/relay.sh installs it
# (relay.sh tls); change the template there. The relay runs with
# --trust-proxy: it takes the client address from the last X-Forwarded-For
# entry, which nginx appends.

server {
    listen 80;
    listen [::]:80;
    server_name @DOMAIN@;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name @DOMAIN@;

    ssl_certificate     /etc/letsencrypt/live/@DOMAIN@/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/@DOMAIN@/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_session_cache   shared:seasoning:10m;
    ssl_session_timeout 1d;
    add_header Strict-Transport-Security "max-age=31536000" always;

    # The relay's own request limit is 128 KiB.
    client_max_body_size 256k;

    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

    # Session event streams (Server-Sent Events): never buffer, and keep
    # idle streams open; the relay sends a keep-alive every 15 s.
    location ~ ^/v1/projects/[^/]+/sessions/[^/]+/events$ {
        proxy_pass http://127.0.0.1:@PORT@;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }

    location /v1/ {
        proxy_pass http://127.0.0.1:@PORT@;
        proxy_read_timeout 60s;
    }

    location = /healthz { proxy_pass http://127.0.0.1:@PORT@; }
    location = /join    { proxy_pass http://127.0.0.1:@PORT@; }

    location / { return 404; }
}
EOF
}

# --- checks and helpers ------------------------------------------------------

valid_port() { [[ $1 =~ ^[0-9]{1,5}$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
valid_domain() { [[ $1 =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$ ]]; }
valid_email() { [[ $1 =~ ^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$ ]]; }
# A version as the installer names its files: semver without the leading v.
valid_version() { [[ $1 =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; }

# Accepts 0.1.0 or v0.1.0; prints the release tag (v0.1.0).
version_tag() {
	local v=${1#v}
	valid_version "$v" || die "'$1' is not a version such as v0.1.0"
	printf 'v%s' "$v"
}

require_root() {
	[ "$(id -u)" = 0 ] && return 0
	if [ -f "$0" ] && command -v sudo >/dev/null 2>&1; then
		local keep="" v
		for v in CLOUDFLARE_API_TOKEN SEASONING_INSTALLER_URL SEASONING_RELEASES_BASE; do
			if [ -n "${!v:-}" ]; then keep="$keep,$v"; fi
		done
		if [ -n "$keep" ]; then
			exec sudo --preserve-env="${keep#,}" -- bash "$0" "${ORIG_ARGS[@]}"
		fi
		exec sudo -- bash "$0" "${ORIG_ARGS[@]}"
	fi
	die "run this as root: sudo bash relay.sh ${ORIG_ARGS[*]}"
}

require_linux() {
	[ "$(uname -s)" = Linux ] || die "this runs on the Linux server; from your machine use: relay.sh --ssh HOST ${ORIG_ARGS[*]}"
}

require_server() {
	require_linux
	require_root
	command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl was not found)"
	# Two runs at once (an update during a backup) would trip over each other.
	exec 9>/run/lock/seasoning-relay.lock || die "cannot open /run/lock/seasoning-relay.lock"
	flock -n 9 || die "another relay.sh is running on this server; wait for it to finish"
}

require_installed() {
	[ -f "$UNIT" ] || die "the relay isn't installed here ($UNIT is missing); run: relay.sh install"
}

have_tty() { (exec </dev/tty) 2>/dev/null; }

confirm() {
	local answer
	[ "$YES" = 1 ] && return 0
	have_tty || die "$1; pass --yes to confirm"
	printf '%s [y/N] ' "$1" >/dev/tty
	read -r answer </dev/tty || answer=""
	case "$answer" in
		y | Y | yes | YES) ;;
		*) die "cancelled" ;;
	esac
}

apt_install() {
	local missing=() p
	for p in "$@"; do
		dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'install ok installed' || missing+=("$p")
	done
	[ ${#missing[@]} -eq 0 ] && return 0
	command -v apt-get >/dev/null 2>&1 ||
		die "this needs ${missing[*]}; install them with your package manager (relay.sh installs packages only with apt)"
	step "installing ${missing[*]}"
	DEBIAN_FRONTEND=noninteractive apt-get update -qq || die "apt-get update failed (see above)"
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "${missing[@]}" ||
		die "apt-get install ${missing[*]} failed (see above)"
}

# The port and domain come from what's installed, so servers set up by hand
# are maintained the same way.
installed_port() {
	local p=""
	if [ -f "$UNIT" ]; then
		p=$(sed -n 's/^ExecStart=.*--listen [^ ]*:\([0-9][0-9]*\).*/\1/p' "$UNIT" | head -n 1)
	fi
	printf '%s' "${p:-$DEFAULT_PORT}"
}

installed_domain() {
	[ -f "$SITE" ] || return 0
	awk '$1 == "server_name" { sub(/;$/, "", $2); print $2; exit }' "$SITE"
}

health() { curl -fsS --max-time 3 "http://127.0.0.1:$1/healthz" 2>/dev/null; }

json_field() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\{0,1\}\([^\",}]*\).*/\1/p"; }

# Waits for /healthz, and for the given version when one is given.
wait_healthy() {
	local port=$1 want=${2:-} i json
	for i in $(seq 1 40); do
		if json=$(health "$port") && { [ -z "$want" ] || [ "$(json_field "$json" version)" = "$want" ]; }; then
			return 0
		fi
		[ "$i" = 40 ] || sleep 0.5
	done
	return 1
}

show_failure() {
	warn "the relay isn't answering on 127.0.0.1:$1; its last log lines:"
	journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
}

active_version() {
	local t
	t=$(readlink "$BIN" 2>/dev/null) || return 1
	case "$t" in
		"$VERSIONS_DIR"/*) printf '%s' "${t##*/}" ;;
		*) return 1 ;;
	esac
}

# Installed version names, newest first.
installed_versions() {
	# shellcheck disable=SC2012 # version names have no whitespace
	ls -1t "$VERSIONS_DIR" 2>/dev/null | tr '\n' ' '
}

# Points the seasoning link at a version file, atomically.
switch_to() {
	local tmp="$BIN_DIR/.seasoning.$$.tmp"
	rm -f "$tmp"
	ln -s "$1" "$tmp"
	mv -f "$tmp" "$BIN"
}

# A binary installed by hand (a regular file) is copied into the versions
# directory first, so update can roll back to it.
adopt_binary() {
	[ -f "$BIN" ] && [ ! -L "$BIN" ] || return 0
	local v
	v=$("$BIN" version 2>/dev/null | awk '{ print $2 }') || v=""
	valid_version "$v" || v=dev
	install -d -m 0755 "$VERSIONS_DIR"
	[ -e "$VERSIONS_DIR/$v" ] || install -m 0755 "$BIN" "$VERSIONS_DIR/$v"
	switch_to "$VERSIONS_DIR/$v"
	say "adopted the existing $BIN as version $v"
}

run_installer() {
	local tag=${1:-} tmp
	tmp=$(mktemp)
	curl -fsSL --retry 2 --max-time 60 -o "$tmp" "$INSTALLER_URL" ||
		{ rm -f "$tmp"; die "could not download the installer from $INSTALLER_URL (see above)"; }
	SEASONING_BIN_DIR=$BIN_DIR SEASONING_VERSIONS_DIR=$VERSIONS_DIR SEASONING_INSTALL_HINTS=0 \
		bash "$tmp" ${tag:+"$tag"} || { rm -f "$tmp"; die "the installer failed (see above); nothing was changed"; }
	rm -f "$tmp"
}

# Starts or restarts the relay. Several restarts in a row (an update, then a
# rollback) would otherwise hit systemd's start limit and be refused.
service_do() {
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	systemctl "$1" "$SERVICE"
}

port_in_use() { [ -n "$(ss -Hltn "sport = :$1" 2>/dev/null)" ]; }

# --- backups -----------------------------------------------------------------

# Writes a consistent copy of the database and prints its path. VACUUM INTO
# runs as the service user: opened as root, SQLite could leave root-owned
# -wal/-shm files that the relay can't open.
backup_db() {
	local dir=$1 label=$2 tmp out
	[ -f "$DB" ] || die "there is no database yet ($DB)"
	apt_install sqlite3 >&2
	install -d -m 0700 "$dir"
	out="$dir/relay-$(utc_stamp)-$label.db"
	tmp="$DATA_DIR/.backup-$$.db"
	rm -f "$tmp"
	runuser -u seasoning -- sqlite3 -cmd '.timeout 10000' "$DB" "VACUUM INTO '$tmp'" ||
		{ rm -f "$tmp"; die "backing up $DB failed (see above)"; }
	check_db "$tmp" || { rm -f "$tmp"; die "the backup failed its check: $CHECK"; }
	mv "$tmp" "$out"
	chown root:root "$out"
	chmod 0600 "$out"
	prune_backups "$dir"
	printf '%s' "$out"
}

# Checks a database file in the data directory as the service user and leaves
# it in rollback-journal mode, so it's a single self-contained file.
check_db() {
	CHECK=$(runuser -u seasoning -- sqlite3 "$1" 'PRAGMA journal_mode=DELETE;' 'PRAGMA integrity_check;' 2>&1) || true
	[ "$(printf '%s\n' "$CHECK" | tail -n 1)" = ok ]
}

prune_backups() {
	local n=0 name
	# SC2012: names are relay-<UTC stamp>-<label>.db, without whitespace.
	# shellcheck disable=SC2012
	ls -1t "$1" 2>/dev/null | while IFS= read -r name; do
		case "$name" in relay-*.db) ;; *) continue ;; esac
		n=$((n + 1))
		[ "$n" -le "$BACKUP_KEEP" ] || rm -f "${1:?}/$name"
	done
}

# Replaces the database (relay stopped) and prints where the old one went.
swap_db() {
	local src=$1 aside f
	aside="$DB.replaced-$(utc_stamp)"
	for f in "$DB" "$DB-wal" "$DB-shm"; do
		[ ! -e "$f" ] || mv "$f" "$aside${f#"$DB"}"
	done
	install -o seasoning -g seasoning -m 0600 "$src" "$DB"
	printf '%s' "$aside"
}

# --- commands ----------------------------------------------------------------

cmd_install() {
	require_server
	local port=${PORT:-$(installed_port)} changed=0 json
	valid_port "$port" || die "--port must be 1-65535, got '$port'"
	command -v apt-get >/dev/null 2>&1 && apt_install curl ca-certificates tar sqlite3

	step "binary"
	adopt_binary
	if [ ! -e "$BIN" ] || [ -n "$VERSION" ]; then
		local before tag=""
		before=$(active_version || true)
		if [ -n "$VERSION" ]; then tag=$(version_tag "$VERSION"); fi
		run_installer "$tag"
		[ "$(active_version || true)" = "$before" ] || changed=1
	else
		say "keeping $(active_version || "$BIN") (relay.sh update upgrades it)"
	fi

	step "user and token"
	if ! id -u seasoning >/dev/null 2>&1; then
		useradd --system --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin seasoning
		say "created the system user seasoning"
	fi
	# systemd reads the env file as root, so it stays root-only.
	install -d -m 0700 -o root -g root "$ETC_DIR"
	if [ -s "$ENV_FILE" ] && grep -q '^SEASONING_RELAY_TOKEN=.' "$ENV_FILE"; then
		chown root:root "$ENV_FILE"
		chmod 0600 "$ENV_FILE"
		say "keeping the relay token in $ENV_FILE"
	else
		write_token
		say "created a relay token in $ENV_FILE (print it with: relay.sh token)"
	fi

	step "systemd unit"
	local new
	new=$(render_unit "$port")
	if [ -f "$UNIT" ] && [ "$(cat "$UNIT")" = "$new" ]; then
		say "unchanged: $UNIT"
	else
		if [ -f "$UNIT" ]; then
			cp -p "$UNIT" "$UNIT.bak-$(utc_stamp)"
			say "updated $UNIT (previous copy: $UNIT.bak-*)"
		else
			say "wrote $UNIT"
		fi
		printf '%s\n' "$new" >"$UNIT"
		chmod 0644 "$UNIT"
		systemctl daemon-reload
		changed=1
	fi

	step "service"
	if ! systemctl is-active --quiet "$SERVICE" && port_in_use "$port"; then
		ss -ltnp "sport = :$port" >&2 || true
		die "port $port is taken by the process above; free it or pick another with: relay.sh install --port N"
	fi
	systemctl enable --quiet "$SERVICE"
	if [ "$changed" = 1 ] || ! systemctl is-active --quiet "$SERVICE"; then
		service_do restart
	fi
	wait_healthy "$port" || {
		show_failure "$port"
		die "the relay didn't become healthy"
	}
	json=$(health "$port")
	say "relay $(json_field "$json" version) is running on 127.0.0.1:$port"
	local domain
	domain=$(installed_domain)
	if [ -z "$domain" ]; then
		say "next: relay.sh tls --domain relay.example.com --email you@example.com"
	else
		step "nginx"
		write_site "$domain" "$port"
	fi
}

write_token() {
	local token
	token=$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')
	(umask 077 && printf 'SEASONING_RELAY_TOKEN=%s\n' "$token" >"$ENV_FILE.tmp")
	mv -f "$ENV_FILE.tmp" "$ENV_FILE"
}

cmd_tls() {
	require_server
	require_installed
	local domain=${DOMAIN:-$(installed_domain)} port
	port=$(installed_port)
	[ -n "$domain" ] || die "--domain is required, e.g. relay.sh tls --domain relay.example.com --email you@example.com"
	valid_domain "$domain" || die "'$domain' is not a hostname such as relay.example.com"
	[ -z "$EMAIL" ] || valid_email "$EMAIL" || die "'$EMAIL' is not an email address"
	valid_port "$DNS_WAIT" && [ "$DNS_WAIT" -ge 10 ] || die "--dns-wait must be at least 10 seconds"
	apt_install nginx certbot python3-certbot-dns-cloudflare

	step "certificate"
	local live=/etc/letsencrypt/live/$domain
	if [ -s "$live/fullchain.pem" ]; then
		say "have a certificate for $domain (certbot's timer renews it)"
	else
		[ -n "$EMAIL" ] || die "--email is required for the first certificate (Let's Encrypt sends expiry warnings there)"
		cloudflare_credentials
		certbot certonly --non-interactive \
			--dns-cloudflare --dns-cloudflare-credentials "$CF_INI" \
			--dns-cloudflare-propagation-seconds "$DNS_WAIT" \
			-d "$domain" --email "$EMAIL" --agree-tos --no-eff-email \
			--deploy-hook "systemctl reload nginx" ||
			die "certbot failed (see above). If the TXT record wasn't found, retry with --dns-wait 60; if Cloudflare refused the token, it needs Zone DNS Edit on this zone (set CLOUDFLARE_API_TOKEN to replace it)"
	fi

	step "nginx"
	write_site "$domain" "$port"

	step "check"
	local json
	# nginx binds new listeners after `systemctl reload` returns.
	json=$(curl -fsS --max-time 10 --retry 10 --retry-delay 1 --retry-connrefused \
		--resolve "$domain:443:127.0.0.1" "https://$domain/healthz") ||
		die "https://$domain/healthz failed through nginx on this server (see above)"
	say "https://$domain is serving relay $(json_field "$json" version)"
	if [ -e /etc/nginx/sites-enabled/default ]; then
		say "note: nginx's default site still answers requests for other names (rm /etc/nginx/sites-enabled/default to drop it)"
	fi
	say "DNS must point $domain at this server with Cloudflare's proxy off (DNS only)."
	say "check renewals with: certbot renew --dry-run"
	say "create a project from your machine: SEASONING_RELAY_TOKEN=… seasoning init --relay https://$domain (token: relay.sh token)"
}

# Writes the nginx site for domain and port, and reloads nginx when it changed.
write_site() {
	local domain=$1 port=$2 new prev="" out
	new=$(render_nginx "$domain" "$port")
	if [ -f "$SITE" ] && [ "$(cat "$SITE")" = "$new" ] && [ -L "$SITE_LINK" ]; then
		say "unchanged: $SITE"
		return 0
	fi
	if [ -f "$SITE" ]; then
		prev="$SITE.bak-$(utc_stamp)"
		cp -p "$SITE" "$prev"
	fi
	printf '%s\n' "$new" >"$SITE"
	ln -sfn "$SITE" "$SITE_LINK"
	if ! out=$(nginx -t 2>&1); then
		printf '%s\n' "$out" >&2
		if [ -n "$prev" ]; then mv -f "$prev" "$SITE"; else rm -f "$SITE" "$SITE_LINK"; fi
		die "nginx rejected the configuration (above); the previous site was restored"
	fi
	systemctl enable --quiet nginx
	if systemctl is-active --quiet nginx; then systemctl reload nginx; else systemctl start nginx; fi
	say "wrote $SITE${prev:+ (previous copy: $prev)}"
}

cloudflare_credentials() {
	local token=${CLOUDFLARE_API_TOKEN:-}
	if [ -z "$token" ] && [ -s "$CF_INI" ]; then
		say "using the Cloudflare token in $CF_INI"
		return 0
	fi
	if [ -z "$token" ]; then
		have_tty || die "no Cloudflare API token: set CLOUDFLARE_API_TOKEN, or run in a terminal to be asked"
		say "Create a Cloudflare API token from the \"Edit zone DNS\" template, limited to this zone."
		printf 'Cloudflare API token (not shown): ' >/dev/tty
		read -rs token </dev/tty || token=""
		printf '\n' >/dev/tty
	fi
	[[ $token =~ ^[[:graph:]]{20,}$ ]] || die "that doesn't look like a Cloudflare API token"
	install -d -m 0700 "$CF_DIR"
	(umask 077 && printf 'dns_cloudflare_api_token = %s\n' "$token" >"$CF_INI.tmp")
	mv -f "$CF_INI.tmp" "$CF_INI"
	say "stored the Cloudflare token in $CF_INI (root only)"
}

cmd_update() {
	require_server
	require_installed
	local port prev_link prev_ver json want=""
	port=$(installed_port)
	adopt_binary
	prev_link=$(readlink "$BIN") || die "$BIN is missing; run: relay.sh install"
	prev_ver=$(active_version || echo unknown)
	if [ -n "$VERSION" ]; then
		want=$(version_tag "$VERSION")
		if [ "v$prev_ver" = "$want" ] && wait_healthy "$port" "$prev_ver"; then
			say "already running $prev_ver"
			return 0
		fi
	fi

	step "download"
	run_installer "$want"
	local new_ver
	new_ver=$(active_version) || die "the installer didn't leave $BIN pointing into $VERSIONS_DIR"
	if [ "$new_ver" = "$prev_ver" ] && wait_healthy "$port" "$new_ver"; then
		say "already running the latest release, $new_ver"
		return 0
	fi

	step "restart on $new_ver"
	systemctl stop "$SERVICE"
	local backup=""
	if [ -f "$DB" ]; then
		if ! backup=$(backup_db "$BACKUP_DIR" "before-$new_ver"); then
			switch_to "$prev_link"
			service_do start
			die "the backup before the update failed (above), so the relay stays on $prev_ver"
		fi
		say "backed up the database to $backup"
	fi
	service_do start
	if wait_healthy "$port" "$new_ver"; then
		say "updated $prev_ver → $new_ver"
		return 0
	fi

	show_failure "$port"
	step "rolling back to $prev_ver"
	systemctl stop "$SERVICE"
	if [ -n "$backup" ]; then
		say "restored the database; the one $new_ver left is at $(swap_db "$backup")"
	fi
	switch_to "$prev_link"
	# Gone, so rollback and the installer's pruning never pick it.
	rm -f "${VERSIONS_DIR:?}/$new_ver"
	service_do start
	wait_healthy "$port" || { show_failure "$port"; die "the rollback to $prev_ver didn't come up either; see the log above"; }
	die "$new_ver didn't become healthy, so the relay is back on $prev_ver (log above)"
}

cmd_rollback() {
	require_server
	require_installed
	local port cur target="" name
	port=$(installed_port)
	cur=$(active_version) || die "$BIN doesn't point into $VERSIONS_DIR; nothing to roll back to"
	if [ -n "$VERSION" ]; then
		target=${VERSION#v}
		[ -f "$VERSIONS_DIR/$target" ] || die "version $target isn't installed; installed: $(installed_versions)"
	else
		# The newest other version: the one active before the last update.
		# shellcheck disable=SC2012 # version names have no whitespace
		while IFS= read -r name; do
			if [ "$name" != "$cur" ] && [ -f "$VERSIONS_DIR/$name" ] && [ ! -L "$VERSIONS_DIR/$name" ]; then
				target=$name
				break
			fi
		done < <(ls -1t "$VERSIONS_DIR")
		[ -n "$target" ] || die "no other version is installed in $VERSIONS_DIR"
	fi
	[ "$target" != "$cur" ] || die "$cur is already active"

	step "switching $cur → $target"
	switch_to "$VERSIONS_DIR/$target"
	service_do restart
	if wait_healthy "$port" "$target"; then
		say "rolled back to $target"
		return 0
	fi
	show_failure "$port"
	switch_to "$VERSIONS_DIR/$cur"
	service_do restart
	wait_healthy "$port" || true
	local hint=""
	hint=$(find "$BACKUP_DIR" -maxdepth 1 -name "relay-*-before-$cur.db" 2>/dev/null | sort | tail -n 1 || true)
	die "$target didn't start, so the relay is back on $cur. If the log says the schema is newer, $cur migrated the database; restore the backup taken before that update${hint:+ ($hint)} with relay.sh restore, then roll back"
}

cmd_backup() {
	require_server
	require_installed
	local out
	out=$(backup_db "${DIR:-$BACKUP_DIR}" manual)
	say "$out ($(du -h "$out" | cut -f1))"
}

cmd_restore() {
	require_server
	require_installed
	local file=${ARG:-} port aside
	[ -n "$file" ] || die "usage: relay.sh restore FILE (backups are in $BACKUP_DIR)"
	[ -f "$file" ] || die "$file doesn't exist"
	apt_install sqlite3
	local tmp="$DATA_DIR/.restore-$$.db"
	install -o seasoning -g seasoning -m 0600 "$file" "$tmp"
	check_db "$tmp" || { rm -f "$tmp"; die "$file isn't a healthy SQLite database: $CHECK"; }
	confirm "Replace the relay database with $file? Messages since that backup are lost." || { rm -f "$tmp"; exit 1; }
	port=$(installed_port)
	systemctl stop "$SERVICE"
	aside=$(swap_db "$tmp")
	rm -f "$tmp"
	service_do start
	wait_healthy "$port" || { show_failure "$port"; die "the relay didn't start on the restored database; the previous one is at $aside"; }
	say "restored $file; the previous database is at $aside"
}

cmd_token() {
	require_server
	[ -s "$ENV_FILE" ] || die "$ENV_FILE is missing; run: relay.sh install"
	if [ "$ROTATE" = 1 ]; then
		confirm "Replace the relay token? Only creating projects needs it; existing projects keep working."
		write_token
		service_do restart
		wait_healthy "$(installed_port)" || { show_failure "$(installed_port)"; die "the relay didn't come back after the token change"; }
		say "new token (give it only to people who may create projects):" >&2
	fi
	sed -n 's/^SEASONING_RELAY_TOKEN=//p' "$ENV_FILE"
}

cmd_status() {
	require_server
	require_installed
	local port domain json bad=0 v
	port=$(installed_port)
	domain=$(installed_domain)
	printf '%-12s %s\n' service "$(systemctl is-active "$SERVICE" || true) ($(systemctl is-enabled "$SERVICE" 2>/dev/null || true))"
	printf '%-12s %s\n' binary "$(active_version || echo "$BIN (not versioned)") — installed: $(installed_versions)"
	if json=$(health "$port"); then
		printf '%-12s %s\n' health "ok on 127.0.0.1:$port: version $(json_field "$json" version), schema $(json_field "$json" schema)"
	else
		printf '%-12s %s\n' health "FAILING on 127.0.0.1:$port"
		bad=1
	fi
	if [ -n "$domain" ]; then
		if curl -fsS --max-time 10 -o /dev/null "https://$domain/healthz"; then
			printf '%-12s %s\n' public "ok: https://$domain"
		else
			printf '%-12s %s\n' public "FAILING: https://$domain/healthz (DNS, firewall or nginx)"
			bad=1
		fi
		local pem=/etc/letsencrypt/live/$domain/fullchain.pem end days
		if [ -r "$pem" ] && end=$(openssl x509 -enddate -noout -in "$pem" 2>/dev/null); then
			end=${end#notAfter=}
			days=$((($(date -u -d "$end" +%s) - $(date -u +%s)) / 86400))
			printf '%-12s %s\n' certificate "expires $(date -u -d "$end" +%Y-%m-%d) UTC ($days days)"
			[ "$days" -ge 14 ] || { warn "the certificate expires in $days days; check: certbot renew --dry-run"; bad=1; }
		fi
	else
		printf '%-12s %s\n' public "no nginx site (relay.sh tls sets one up)"
	fi
	if [ -f "$DB" ]; then
		printf '%-12s %s\n' database "$DB ($(du -ch "$DB"* | tail -n 1 | cut -f1))"
	fi
	v=$(find "$BACKUP_DIR" -maxdepth 1 -name 'relay-*.db' 2>/dev/null | sort | tail -n 1 || true)
	printf '%-12s %s\n' backups "${v:-none} ($(find "$BACKUP_DIR" -maxdepth 1 -name 'relay-*.db' 2>/dev/null | wc -l | tr -d ' ') kept in $BACKUP_DIR)"
	v=$(journalctl -u "$SERVICE" -p warning --since=-24h -q --no-pager 2>/dev/null | wc -l | tr -d ' ' || true)
	printf '%-12s %s\n' warnings "$v in the last 24 h (relay.sh logs)"
	return "$bad"
}

cmd_logs() {
	require_linux
	require_root
	if [ ${#REST[@]} -eq 0 ]; then
		exec journalctl -u "$SERVICE" -n 100 --no-pager
	fi
	exec journalctl -u "$SERVICE" "${REST[@]}"
}

cmd_uninstall() {
	require_server
	local domain
	domain=$(installed_domain)
	if [ "$PURGE" = 1 ]; then
		confirm "Remove the relay and delete its database, token and backups?"
	else
		confirm "Remove the relay? Its database, token and backups stay."
	fi
	if [ -f "$UNIT" ]; then
		systemctl disable --now --quiet "$SERVICE" || true
		rm -f "$UNIT" "$UNIT".bak-*
		systemctl daemon-reload
		say "removed the service"
	fi
	if [ -e "$SITE" ] || [ -L "$SITE_LINK" ]; then
		rm -f "$SITE_LINK" "$SITE" "$SITE".bak-*
		if command -v nginx >/dev/null 2>&1 && systemctl is-active --quiet nginx && nginx -t 2>/dev/null; then
			systemctl reload nginx
		fi
		say "removed the nginx site"
	fi
	if [ -L "$BIN" ] && [ -n "$(active_version || true)" ]; then rm -f "$BIN"; fi
	rm -rf "$VERSIONS_DIR"
	rmdir /opt/seasoning 2>/dev/null || true
	say "removed the binaries"
	if [ "$PURGE" = 1 ]; then
		rm -rf "$DATA_DIR" "$ETC_DIR" "$BACKUP_DIR"
		if id -u seasoning >/dev/null 2>&1; then userdel seasoning; fi
		say "deleted the database, token, backups and the seasoning user"
	else
		say "kept $DATA_DIR, $ETC_DIR and $BACKUP_DIR (uninstall --purge deletes them)"
	fi
	say "left alone: nginx, certbot, the certificate and $CF_INI${domain:+ (certbot delete --cert-name $domain removes the certificate)}"
}

cmd_render() {
	case "${ARG:-}" in
		unit) render_unit "${PORT:-$DEFAULT_PORT}" ;;
		nginx)
			local d=${DOMAIN:-relay.example.com}
			valid_domain "$d" || die "'$d' is not a hostname such as relay.example.com"
			render_nginx "$d" "${PORT:-$DEFAULT_PORT}"
			;;
		*) die "usage: relay.sh render unit|nginx [--port N] [--domain HOST]" ;;
	esac
}

# --- remote ------------------------------------------------------------------

# Single-quotes a word for the remote POSIX shell.
sq() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

run_remote() {
	local host=$1 payload args="" a ssh_cmd
	shift
	[ -f "$0" ] || die "--ssh needs the script as a file (run it as deploy/relay.sh, not through a pipe)"
	[ $# -gt 0 ] || die "usage: relay.sh --ssh HOST COMMAND [options]"
	payload=$(base64 <"$0" | tr -d '\n')
	for a in "$@"; do args="$args $(sq "$a")"; done
	# One connection with a terminal, for sudo's password and the token
	# prompt; the script travels in the command line, not on stdin.
	local remote="f=\$(mktemp) && trap 'rm -f \"\$f\"' EXIT && printf '%s' '$payload' | base64 -d >\"\$f\" && if [ \"\$(id -u)\" = 0 ]; then bash \"\$f\"$args; else sudo bash \"\$f\"$args; fi"
	read -r -a ssh_cmd <<<"${SEASONING_SSH:-ssh}"
	if [ -t 0 ] && [ -t 1 ]; then
		exec "${ssh_cmd[@]}" -t "$host" "bash -c $(sq "$remote")"
	fi
	exec "${ssh_cmd[@]}" "$host" "bash -c $(sq "$remote")"
}

# --- main ----------------------------------------------------------------------

main() {
	if [ "${1:-}" = --ssh ]; then
		[ -n "${2:-}" ] || die "usage: relay.sh --ssh HOST COMMAND [options]"
		local host=$2
		shift 2
		run_remote "$host" "$@"
	fi

	local cmd=${1:-help}
	[ $# -eq 0 ] || shift
	PORT="" VERSION="" DOMAIN="" EMAIL="" DIR="" DNS_WAIT=30 ROTATE=0 PURGE=0 YES=0 ARG=""
	REST=()
	if [ "$cmd" = logs ]; then
		REST=("$@")
		set --
	fi
	while [ $# -gt 0 ]; do
		case "$1" in
			--port | --version | --domain | --email | --dir | --dns-wait)
				[ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value"
				case "$1" in
					--port) PORT=$2 ;;
					--version) VERSION=$2 ;;
					--domain) DOMAIN=$2 ;;
					--email) EMAIL=$2 ;;
					--dir) DIR=$2 ;;
					--dns-wait) DNS_WAIT=$2 ;;
				esac
				shift 2
				;;
			--rotate) ROTATE=1 && shift ;;
			--purge) PURGE=1 && shift ;;
			--yes | -y) YES=1 && shift ;;
			-h | --help) usage && exit 0 ;;
			-*) die "unknown option $1 (see relay.sh help)" ;;
			*)
				[ -z "$ARG" ] || die "unexpected argument '$1' (see relay.sh help)"
				ARG=$1 && shift
				;;
		esac
	done
	if [ -n "$PORT" ]; then valid_port "$PORT" || die "--port must be 1-65535, got '$PORT'"; fi
	if [ -n "$VERSION" ] && ! valid_version "${VERSION#v}" && [ "$VERSION" != dev ]; then
		die "--version must be a version such as v0.1.0, got '$VERSION'"
	fi
	case "$cmd" in
		render | restore) ;;
		*) [ -z "$ARG" ] || die "unexpected argument '$ARG' (see relay.sh help)" ;;
	esac

	case "$cmd" in
		install) cmd_install ;;
		tls) cmd_tls ;;
		update) cmd_update ;;
		rollback) cmd_rollback ;;
		backup) cmd_backup ;;
		restore) cmd_restore ;;
		token) cmd_token ;;
		status) cmd_status ;;
		logs) cmd_logs ;;
		uninstall) cmd_uninstall ;;
		render) cmd_render ;;
		help | -h | --help) usage ;;
		*) die "unknown command '$cmd' (see relay.sh help)" ;;
	esac
}

main "$@"
