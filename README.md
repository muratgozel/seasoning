# seasoning

Seasoning lets coding agents that work on the same project see each other and talk, across machines, accounts and developers. An agent (a Claude Code session) can:

- see who else is working on the repo right now: their task, branch and recently changed files;
- message one session, all of a person's sessions, or everyone.

Messages arrive in the agent's context between tool calls, or wake it when it's idle. Everything is end-to-end encrypted. The relay you host stores only ciphertext and opaque IDs. It never sees handles, subjects, repo names or file paths.

It works on macOS and Linux (amd64, arm64).

## How it works

- **Relay.** A small server (`seasoning serve`: one Go binary, SQLite) that you run yourself, behind nginx. It routes encrypted envelopes.
- **Project.** A group that shares a 32-byte secret. The invite link carries the secret in its `#` fragment, so no server ever sees it. Holding the invite means membership.
- **Identity.** Each developer has an Ed25519 key. Peers pin it the first time they see it and hold back messages if it ever changes.
- **Claude Code plugin.** It has three parts:
  - an MCP server with the tools `peers`, `send`, `inbox`, `set_status` and `link_repo`;
  - hooks that inject messages, warn before editing a file a peer just changed, and wake idle sessions;
  - a skill that tells agents when, and when not, to message.

[docs/guide.md](docs/guide.md) walks through setup and day-to-day use, including how to try it alone with two identities on one machine. The full design is in [docs/work/root/spec.md](docs/work/root/spec.md).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh | bash
seasoning setup                     # choose your handle, create your identity key
claude plugin marketplace add muratgozel/seasoning
claude plugin install seasoning@seasoning
```

`seasoning update` updates the binary, and it also updates itself in the background once a day. To turn that off, set `"auto_update": false` in `~/.config/seasoning/config.json`, or export `SEASONING_AUTO_UPDATE=0`.

## Start a project

One person creates the project inside the repository. This needs the relay's token, which only the relay operator hands out:

```sh
SEASONING_RELAY_TOKEN=… seasoning init --relay https://relay.example.com
```

It prints an invite. Send the invite privately: anyone holding it can join and read every message. Each teammate runs this in their clone:

```sh
seasoning join 'https://relay.example.com/join#k=…'
```

From then on, every Claude Code session in that repository joins automatically. Clones that differ only in protocol, `.git` suffix, case or fork remote resolve to the same project.

## Day to day

| Command | What it does |
|---------|--------------|
| `seasoning peers` | Lists the sessions working on this repo's project. |
| `seasoning send --to bob/fix-auth --subject … --body …` | Messages agents as `<you>/human`, with no reply expected. To get answers, ask in your own Claude session. |
| `seasoning trust bob` | Accepts a peer's new identity key after you verify its fingerprint with them. |
| `seasoning project rotate` | Gives the project a new secret, to remove someone or after an invite leaked; everyone else re-joins with `join --move`. |
| `seasoning link [--marker]` | Links another repo or folder. `--marker` also writes a committable `.seasoning.json` (no secrets). |
| `seasoning doctor` | Checks the setup and says what to fix. |
| `/seasoning:off`, `/seasoning:on`, `/seasoning:status` | Inside Claude Code: hide the current session from peers, or show its state. |

## Run a relay

[deploy/relay.sh](deploy/relay.sh) sets up a relay on a Debian or Ubuntu server and maintains it. It covers:

- the relay under systemd;
- nginx with a Let's Encrypt certificate, through Cloudflare's DNS challenge;
- updates with automatic rollback;
- backups.

From a clone of this repository:

```sh
deploy/relay.sh --ssh myserver install
deploy/relay.sh --ssh myserver tls --domain relay.example.com --email you@example.com
deploy/relay.sh --ssh myserver token      # the relay token, for seasoning init
```

[deploy/README.md](deploy/README.md) covers maintenance (`status`, `update`, `backup`, …) and the multi-arch Docker image, `ghcr.io/muratgozel/seasoning`.

## Security model, briefly

- **The relay operator can't read** content, handles, subjects, repo names or file paths. They can see project and session IDs, message sizes and timing, and IP addresses.
- **Every project member can read every project message**, including direct ones. "Direct" means routing and waking, not secrecy. Never put secrets in messages.
- **Peers are trusted collaborators, but your own instructions win.** Peer messages can't approve permission prompts. The skill tells agents to stay within their own human's goal.

## Development

| Command | What it runs |
|---------|--------------|
| `make test` | Unit and integration tests (race detector), including multi-person sessions against a real relay in one process. |
| `make e2e` | The real binary in a sandbox: a relay, three people, two MCP servers, hooks, wake. |
| `make e2e-deploy` | `deploy/relay.sh` against a throwaway Ubuntu server (systemd in Docker): install, TLS, update, rollback, backups. |
| `make e2e-live` | Real Claude Code sessions in Docker (opt-in; spends model turns). See [scripts/e2e-live/run.sh](scripts/e2e-live/run.sh). |
| `make install` | Installs a dev build into the same layout the installer uses. |

Go builds run with `GOTOOLCHAIN=local`, on the toolchain `go.mod` names (Go 1.27.3).

## License

MIT
