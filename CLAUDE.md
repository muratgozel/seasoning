# seasoning: notes for agents

seasoning lets Claude Code sessions working on the same project see each other and exchange end-to-end encrypted messages. It has three parts: a self-hosted relay, a Claude Code plugin, and a CLI. All three are one Go binary.

Read before changing behavior:
- [docs/work/root/spec.md](docs/work/root/spec.md): the design and its decision log (D1–D26).
  - §23 has Claude Code behavior verified by experiment.
  - §24 has as-built notes on hooks, delivery, release and deploy. Read the part you're touching.
- [docs/guide.md](docs/guide.md): usage.
- [deploy/README.md](deploy/README.md): running a relay.

## Layout

| Path | What |
|------|------|
| `cmd/seasoning`, `internal/cli` | Entry point and commands. |
| `internal/relay`, `relayapi`, `relayclient` | Relay server (SQLite, REST, SSE) and its client. |
| `internal/agent`, `hooks`, `mcpserver`, `inject`, `runtime`, `session`, `roster` | The Claude Code side: hooks, MCP tools, message injection, wake. |
| `internal/keys`, `envelope`, `jcs`, `invite` | Crypto, message format, canonical JSON, invites. |
| `internal/config`, `localdb`, `project`, `gitremote`, `paths`, `fsutil`, `errlog`, `selfupdate`, `version` | Local state, repo-to-project mapping, self-update. |
| `plugin/` | The Claude Code plugin; the marketplace is in `.claude-plugin/marketplace.json`. |
| `deploy/` | `relay.sh`, plus the systemd unit and nginx site it renders, and the image's Dockerfile. |
| `scripts/` | `install.sh`, `e2e.sh`, `e2e-deploy/`, `e2e-live/`. |

## Build and test

| Command | When |
|---------|------|
| `make lint test e2e` | Always, before pushing. |
| `make e2e-deploy` | After changing `deploy/` or `scripts/install.sh`. It needs Docker. |
| `make e2e-live` | After changing hooks, the plugin or delivery. It spends model turns, so ask first. |
| `claude plugin validate plugin && claude plugin validate .` | After changing `plugin/` or the marketplace. |

- **Go:** builds use `GOTOOLCHAIN=local` with the `toolchain` line in `go.mod`. Install that Go from go.dev; `make` fails on an older one.
- **e2e-live** needs a Docker volume with a Claude Code login. `scripts/e2e-live/run.sh` has the one-time login command; select the volume with `E2E_CLAUDE_VOLUME=<volume>`.
  - If Claude Code offers usage credits or extra usage, dismiss the dialog with Escape. Never accept it.
  - When an account hits its usage limit, log another one into a new volume.
- **CI runs on Linux.** To catch Linux-only failures before pushing (GNU vs BSD tools, a missing git identity), replay the go job:

  ```sh
  docker run --rm -v "$PWD:/src:ro" golang:<toolchain> bash -c 'mkdir /w && cp -r /src/. /w && cd /w && rm -rf bin spikes &&
    GOTOOLCHAIN=local go test -race ./... && GOTOOLCHAIN=local make e2e'
  ```

## Conventions

- **Public repo:** never commit real hostnames, IPs, relay URLs, tokens, invites or emails (spec §19). Fixtures use `alice`, `bob`, `example.com` and `relay.example.com`. Grep staged content before each commit.
- **Git:**
  - Branches are named `<user>/<name>`, and commits use conventional messages.
  - Commits are signed through the maintainer's git config; don't override the identity or signing.
  - Commit and push only when asked.
- **Errors** say what to do next and never leak secrets (spec §17). Validate input at the boundary.
- **Comments** only explain behavior that isn't obvious, especially anything a later agent could break.
- **Times** are stored in UTC (spec §18).
- **Docs** are terse, because the readers are developers.
- **Unicode escapes:** `\uXXXX` written by an agent's file tools can land in the source as the literal character. Build such strings programmatically, as `esc()` does in `internal/jcs/jcs_test.go`.
- **Shell:**
  - Scripts use bash with `set -euo pipefail`.
  - They must pass shellcheck 0.9 through 0.11. CI's version is older and fails on SC2015, so write `if` statements, not `a && b || c`.
  - `deploy/relay.sh`'s `help`, `render` and `--ssh` paths run on macOS's bash 3.2.
  - For portable `stat`, try the GNU form first: `stat -c %a f 2>/dev/null || stat -f %Lp f`.
  - Inside `$(…)`, send helper chatter to stderr.
- **Contracts to keep in sync:**
  - `scripts/install.sh`, `internal/selfupdate` and `.goreleaser.yaml` share the asset names (`seasoning_<ver>_<os>_<arch>.tar.gz`, binary at the archive root, `checksums.txt`) and the versions-dir layout.
  - `inject.RewakePrefix` must equal `rewakeMessage` in `plugin/hooks/hooks.json`.
  - The plugin's tools are named `mcp__plugin_seasoning_seasoning__*`, and the plugin passes `--plugin-proto 1` (spec §15.3).
  - Relay `/v1` and envelope changes are additive only.
- **Rendered templates:** `deploy/systemd/seasoning-relay.service` and `deploy/nginx/seasoning.conf` are renders of templates in `deploy/relay.sh`, and CI diffs them. Edit the script, then regenerate:
  `bash deploy/relay.sh render unit > deploy/systemd/seasoning-relay.service && bash deploy/relay.sh render nginx > deploy/nginx/seasoning.conf`.

## Releasing

1. Choose `vX.Y.Z`. A prerelease is `vX.Y.Z-rc.N`; the installer, auto-update and the image's `latest` tag all skip prereleases.
2. Set `"version": "X.Y.Z"` in `plugin/.claude-plugin/plugin.json`. The release workflow fails otherwise, and Claude Code updates an installed plugin only when this version changes.
3. Run the checks above, commit, push `main`, and wait for CI: `gh run watch <id> --exit-status`.
4. Tag and push: `git tag -s vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`.
5. `.github/workflows/release.yml` runs the tests, govulncheck and goreleaser. It publishes:
   - the GitHub release, with 4 archives and checksums;
   - `ghcr.io/muratgozel/seasoning:X.Y.Z`, plus `latest` (amd64 and arm64).
6. goreleaser's changelog lists only `feat`/`fix` commits. When that's thin, rewrite the notes with `gh release edit vX.Y.Z --notes-file <file>`.
7. Verify as a user would:
   - the installer, in a clean `ubuntu` container and on macOS into a temporary `SEASONING_BIN_DIR`/`SEASONING_VERSIONS_DIR`;
   - `DOCKER_CONFIG=$(mktemp -d) docker pull ghcr.io/muratgozel/seasoning:X.Y.Z`, then `/healthz`;
   - `claude plugin marketplace add muratgozel/seasoning && claude plugin install seasoning@seasoning` in the `seasoning-e2e:live` image.
8. Relays don't update themselves. Run `deploy/relay.sh --ssh HOST update`, after asking the server's owner.

- Never move or delete a published tag; ship fixes as the next patch.
- For a local dry run, `goreleaser release --snapshot --clean` needs a git repo with a remote, or it ignores the tag. Docker Desktop on Apple silicon may lack amd64 emulation; check foreign-arch binaries with `file` instead of running them.

## Toolchain and dependencies

- **Bump:**
  1. `go mod edit -toolchain=goX.Y.Z`, and `-go=X.Y.0` if a dependency needs a newer minimum.
  2. `go get <module>@<version> && go mod tidy`.
  3. Update the Go line in the README.
- **setup-go reads only the `go` line.** Both workflows read the `toolchain` line themselves and pass it as `go-version`; keep that step.
- **govulncheck** (pinned to v1.9.0) runs in CI and in releases. It fails on vulnerabilities the code can reach; for standard-library ones, bump the toolchain.
- **Images:** goreleaser uses `dockers_v2`. `deploy/Dockerfile` copies `$TARGETPLATFORM/seasoning`, and its prep stage runs on `$BUILDPLATFORM`, so no QEMU is needed.

## Relays in production

- **`deploy/relay.sh` is the only supported way to change a server.** It reads the port and domain from the installed unit and nginx site, and it adopts hand-installed binaries.
- **Ask before running anything against a real server.** Other services may share it: check ports with `ss -ltnp` before choosing one, and leave everything else alone.
- **Standard setup:** the relay on `127.0.0.1:8090` with `--trust-proxy`, behind nginx. Cloudflare records are DNS only, because proxying cuts the long-lived event streams.

## Claude Code behavior worth knowing

The verified details are in spec §23 and §24. The ones that bite most often:
- **Hook output:** a hook's `additionalContext` reaches the model, and its `systemMessage` reaches the human.
- **Wake under `claude -p`:** an `asyncRewake` hook runs synchronously there. The waiter checks `CLAUDE_CODE_SESSION_ATTENDED`.
- **`/clear`** keeps the MCP process but gives the session a new `session_id`.
- **Worktrees:** per the docs, hook `cwd` follows EnterWorktree and `cd`, so a session's starting root goes stale. File paths are relative to the file's own work tree (`hooks.worktree`).
- **Tests:** deny the built-in `SendMessage` and `ListAgents` tools, or agents use them instead of seasoning.
