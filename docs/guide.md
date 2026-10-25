# Using seasoning

Assumes a running relay ([deploy/README.md](../deploy/README.md)).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh | bash   # or make install in a clone
seasoning setup                                # handle + identity key
claude plugin marketplace add muratgozel/seasoning   # or a clone's path
claude plugin install seasoning@seasoning      # then restart Claude Code
seasoning doctor
```

## Project

```sh
SEASONING_RELAY_TOKEN=… seasoning init --relay https://relay.example.com   # prints the invite (a secret)
seasoning join '<invite>'                       # in a teammate's clone
seasoning link --project <name>                 # add another repo or folder
seasoning peers
```

Repos match by any git remote (forks and worktrees included), or by path via `link`.

## In Claude Code

Sessions in a linked repo join automatically.

- **Tools:** `peers`, `send`, `inbox`, `set_status`, `link_repo`.
- **Addresses:** `bob/<label>`, `bob/*`, `*`.
- **Delivery:** messages arrive between tool calls. A message to a specific session wakes it when idle if it expects a reply or answers that session's question; the sender's `wake` overrides. FYIs, `bob/*` and `*` wait for the next turn unless urgent. Ended sessions keep messages for 7 days.
- **Purpose:** messages coordinate work so sessions don't overlap or conflict. They are not a progress log; status goes in `set_status`.
- **File warnings:** an agent is warned before editing a file a live peer changed.
- **Commands:** `/seasoning:status`, `/seasoning:off`, `/seasoning:on`. `SEASONING=off claude` skips one session.
- **Permission prompts:** allow `mcp__plugin_seasoning_seasoning` in settings to skip them.

## As a human

```sh
seasoning send --to bob/<label> --subject … --body …   # as <you>/human, no reply expected; --urgent, --body -
```

## Two people on one machine

```sh
B=~/.config/seasoning-bob
SEASONING_CONFIG_DIR=$B seasoning setup --handle bob
git worktree add ../repo-bob && cd ../repo-bob
SEASONING_CONFIG_DIR=$B seasoning join '<invite>'
SEASONING_CONFIG_DIR=$B claude --settings '{"permissions":{"deny":["SendMessage","ListAgents"]}}'
```

The deny rule keeps Claude Code's built-in messaging from short-circuiting the test. Start your own session the same way, without `SEASONING_CONFIG_DIR`.

Things to try:
- label both sessions;
- ask an idle session a question, which should wake it to reply;
- send it an FYI, which should wait for its next turn;
- broadcast an FYI, which should not wake anyone;
- edit a file the other session changed, which should warn, even from another worktree.

## Manage

```sh
seasoning invite                                # print the invite again
seasoning link --marker                         # write a committable .seasoning.json (no secrets)
seasoning trust <handle>                        # accept a peer's new key after verifying its fingerprint
SEASONING_RELAY_TOKEN=… seasoning project rotate   # new secret; others run: seasoning join --move '<new invite>'
seasoning unlink
```

## Troubleshooting

Start with `seasoning doctor`. Hooks log to `~/.local/state/seasoning/errors.log`.

- **"isn't linked"**: run `join` or `link` in that directory, then `/clear`.
- **No session-start line**: the plugin isn't loaded; check `claude plugin list`.
- **Messages held back** (a peer's key changed): run `seasoning trust <handle>`.

## Files

| Path | Contents |
|------|----------|
| `~/.config/seasoning/` | Handle, identity key, project secrets, pins. Private. |
| `~/.local/share/seasoning/local.db` | Decrypted messages. Kept 30 days. |
| `~/.local/state/seasoning/` | `errors.log`, runtime files. |
