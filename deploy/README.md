# Running a relay

[relay.sh](relay.sh) sets up and maintains a relay on a Debian or Ubuntu
server with systemd (tested on Ubuntu 26.04):
- the relay as a hardened systemd service on `127.0.0.1:8090`;
- nginx in front of it, with a Let's Encrypt certificate from certbot's
  Cloudflare DNS challenge;
- updates with automatic rollback, and backups.

Every command is idempotent and safe to re-run.

## Before you start

- **DNS:** point an `A` record (and `AAAA` for IPv6) for the relay's hostname
  at the server. Set Cloudflare's proxy to **DNS only** (grey cloud).
  Proxying adds a second TLS hop whose timeouts cut the agents' long-lived
  event streams.
- **Cloudflare API token:** in My Profile → API Tokens → Create Token, use the
  **Edit zone DNS** template, limited to this zone.

## Set up

From your machine, with an SSH host that can sudo. The script copies itself
to the server and runs there:

```sh
deploy/relay.sh --ssh myserver install
deploy/relay.sh --ssh myserver tls --domain relay.example.com --email you@example.com
deploy/relay.sh --ssh myserver token
```

- `tls` asks for the Cloudflare token once and stores it root-only.
- `token` prints the relay token. It is needed only to create projects, so
  give it only to people who may do that.

Or on the server itself:

```sh
curl -fsSLO https://raw.githubusercontent.com/muratgozel/seasoning/main/deploy/relay.sh
sudo bash relay.sh install
sudo bash relay.sh tls --domain relay.example.com --email you@example.com
```

Then create the first project on your machine:

```sh
SEASONING_RELAY_TOKEN=… seasoning init --relay https://relay.example.com
```

## Maintain

| Command | What it does |
|---------|--------------|
| `status` | Shows the service, version, local and public health, certificate expiry, database and backups. Exits non-zero when something fails. |
| `update [--version vX.Y.Z]` | Installs the release, stops the relay, backs up the database, then starts the new version. If `/healthz` doesn't report the new version, it restores the backup and the previous binary. |
| `rollback [--version X.Y.Z]` | Switches to another installed version (the last 3 are kept). |
| `backup [--dir DIR]` | Makes a consistent copy of the database while the relay runs, into `/var/backups/seasoning/`, keeping the newest 10. |
| `restore FILE` | Stops the relay, swaps the database for a backup and starts it again; the replaced database is kept next to it. |
| `token --rotate` | Issues a new relay token. Existing projects keep working. |
| `logs [-f]` | Shows the relay's journal. |
| `uninstall [--purge]` | Removes the service, nginx site and binaries. `--purge` also deletes the database, token, backups and system user. |
| `render unit\|nginx` | Prints the [systemd unit](systemd/seasoning-relay.service) or [nginx site](nginx/seasoning.conf) it installs. |

Prefix each command with `--ssh HOST`, or run it on the server with
`sudo bash relay.sh`.

A few behaviors to know:
- **No self-update:** the relay never updates itself.
- **Restarts:** restarting drops event streams; clients reconnect and resume
  on their own.
- **Older versions and newer databases:** an older relay refuses a database
  that a newer one migrated. To roll back across a schema change, first
  `restore` the `relay-…-before-<version>.db` backup that `update` took.
- **Certificates** renew through certbot's timer. To test renewal, run
  `certbot renew --dry-run`.
- **Servers set up by hand** work too: the script reads the port and domain
  from the installed unit and nginx site. On the first `update`, it adopts a
  binary that was copied in by hand, so rollback works.

## Files on the server

| Path | Contents |
|------|----------|
| `/usr/local/bin/seasoning` → `/opt/seasoning/versions/<version>` | The binary, in the installer's layout. |
| `/etc/seasoning/relay.env` | The relay token. Root only. |
| `/var/lib/seasoning/relay.db` | The database: projects, sessions and messages, all ciphertext. |
| `/var/backups/seasoning/` | Backups. Root only. |
| `/etc/systemd/system/seasoning-relay.service` | The unit. |
| `/etc/nginx/sites-available/seasoning.conf` | The nginx site. |
| `/etc/letsencrypt/cloudflare/credentials.ini` | The Cloudflare token. Root only. |

## Firewall

The relay needs only SSH, HTTP (for the redirect) and HTTPS:

```sh
sudo ufw allow OpenSSH && sudo ufw allow 'Nginx Full' && sudo ufw enable
```

Allow OpenSSH before enabling ufw, or you lock yourself out. Ports that
Docker publishes bypass ufw, so publish them on `127.0.0.1` only.

## Docker

The image runs the same relay. Put your own TLS proxy in front of it; the
[nginx site](nginx/seasoning.conf) shows what it needs.

```sh
(umask 077 && printf 'SEASONING_RELAY_TOKEN=%s\n' "$(openssl rand -base64 32)" >relay.env)
docker run -d --name seasoning-relay --restart unless-stopped --env-file relay.env \
  -v seasoning:/data -p 127.0.0.1:8090:8090 ghcr.io/muratgozel/seasoning \
  serve --listen 0.0.0.0:8090 --db /data/relay.db --trust-proxy
```

## Troubleshooting

| Symptom | Check |
|---------|-------|
| `502 Bad Gateway` | `relay.sh status`, then `relay.sh logs`. |
| `install` says the port is taken | Another program listens on 8090. Pick another port with `relay.sh install --port 8091`; nginx follows. |
| Agents connect but never get messages | Something buffers the event stream. Usually Cloudflare's proxy is on; set the record to DNS only. |
| `tls` fails at certbot | If the TXT record wasn't found, retry with `--dns-wait 60`. If the token was refused, it needs DNS edit on this zone; set `CLOUDFLARE_API_TOKEN` to replace it. |

`make e2e-deploy` runs every command above against a throwaway Ubuntu server in
Docker.
