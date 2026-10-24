#!/bin/sh
# End-to-end check of the real seasoning binary in a throwaway sandbox: a
# relay started with `seasoning serve`, and three people with their own
# config dirs and clones. Nothing outside the sandbox is touched.
#
#   make e2e                 (builds bin/seasoning first)
#   E2E_KEEP=1 make e2e      keep the sandbox even on success
#
# On failure the sandbox is kept and its path printed.
set -eu

BIN="${SEASONING_BIN:-$(pwd)/bin/seasoning}"
[ -x "$BIN" ] || { echo "e2e: $BIN not found; run make build" >&2; exit 1; }

T="$(mktemp -d "${TMPDIR:-/tmp}/seasoning-e2e.XXXXXX")"
relay_pid=""
all_pids="" # every background process started, stopped on exit
cleanup() {
	status=$?
	for pid in $all_pids; do kill "$pid" 2>/dev/null || true; done
	if [ "$status" -ne 0 ]; then
		echo "e2e: FAILED; sandbox kept at $T" >&2
		[ -f "$T/relay.log" ] && { echo "--- relay log (tail)" >&2; tail -20 "$T/relay.log" >&2; }
	elif [ "${E2E_KEEP:-}" = 1 ]; then
		echo "e2e: sandbox kept at $T"
	else
		rm -rf "$T"
	fi
}
trap cleanup EXIT

export HOME="$T/home"
export GIT_CONFIG_GLOBAL="$T/gitconfig"
export XDG_CONFIG_HOME="$T/xdg-config" XDG_DATA_HOME="$T/xdg-data" XDG_STATE_HOME="$T/xdg-state"
mkdir -p "$HOME"
TOKEN="e2e-relay-token-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"

step() { printf '%-60s ' "$1"; }
ok() { echo ok; }
fail() { echo FAIL; echo "  $1" >&2; exit 1; }
# mode FILE prints its permission bits. GNU stat first: its -f means
# "file system" and prints junk before failing on BSD's format.
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# as PERSON DIR ARGS... runs seasoning with PERSON's config dir in DIR.
as() {
	who="$1"; dir="$2"; shift 2
	(cd "$dir" && SEASONING_CONFIG_DIR="$T/people/$who" "$BIN" "$@")
}

clone() {
	mkdir -p "$T/repos/$1"
	git -C "$T/repos/$1" init -q
	git -C "$T/repos/$1" remote add origin "$2"
	echo "$T/repos/$1"
}

# start_relay [ADDR] starts the relay on ADDR (default: a free port) and sets
# RELAY. A restart passes the previous address, since invites embed it.
start_relay() {
	[ -z "$relay_pid" ] || fail "start_relay: a relay is already running (pid $relay_pid)"
	SEASONING_RELAY_TOKEN="$TOKEN" "$BIN" serve --listen "${1:-127.0.0.1:0}" --db "$T/relay.db" >>"$T/relay.log" 2>&1 &
	relay_pid=$!
	all_pids="$all_pids $relay_pid"
	addr=""
	i=0
	while [ $i -lt 50 ]; do
		addr="$(sed -n 's/.*"relay listening","addr":"\([^"]*\)".*/\1/p' "$T/relay.log" | tail -1)"
		[ -n "$addr" ] && break
		sleep 0.1
		i=$((i + 1))
	done
	[ -n "$addr" ] || fail "relay did not start"
	RELAY="http://$addr"
}

stop_relay() {
	kill -TERM "$relay_pid"
	wait "$relay_pid" || fail "relay exited with $? on SIGTERM"
	relay_pid=""
}

step "relay starts and answers /healthz"
start_relay
curl -fsS "$RELAY/healthz" | grep -q '"ok":true' || fail "healthz"
ok

A="$(clone alice git@github.com:example/acme-api.git)"
B="$(clone bob https://github.com/Example/acme-api)"
C="$(clone carol git@github.com:example/acme-api.git)"

step "setup creates identities"
as alice "$A" setup --handle alice >/dev/null
as bob "$B" setup --handle bob >/dev/null
as carol "$C" setup --handle alice >/dev/null
[ "$(mode "$T/people/alice/identity")" = 600 ] || fail "identity file is not 0600"
ok

step "init without the relay token is refused"
if as alice "$A" init --relay "$RELAY" >/dev/null 2>"$T/err"; then fail "init succeeded without a token"; fi
grep -q SEASONING_RELAY_TOKEN "$T/err" || fail "error does not name SEASONING_RELAY_TOKEN: $(cat "$T/err")"
ok

step "init creates the project and prints an invite"
SEASONING_RELAY_TOKEN="$TOKEN" as alice "$A" init --relay "$RELAY" >"$T/init.out"
INVITE="$(grep '/join#k=' "$T/init.out" | grep -v "seasoning join" | head -1 | tr -d ' ')"
[ -n "$INVITE" ] || fail "no invite in: $(cat "$T/init.out")"
[ "$(as alice "$A" invite 2>/dev/null)" = "$INVITE" ] || fail "invite differs from init's"
ok

step "bob joins from a clone with a different remote form"
as bob "$B" join "$INVITE" >"$T/join.out"
grep -q "Members: alice, bob" "$T/join.out" || fail "$(cat "$T/join.out")"
ok

step "a second person can't take the handle alice"
if as carol "$C" join "$INVITE" >/dev/null 2>"$T/err"; then fail "carol joined as alice"; fi
grep -q 'already used' "$T/err" || fail "$(cat "$T/err")"
ok

step "the relay stores no plaintext handles or repo names"
for s in alice bob acme-api github; do
	if grep -qa "$s" "$T/relay.db" "$T/relay.db-wal" 2>/dev/null; then fail "relay.db contains $s"; fi
done
ok

step "relay restarts gracefully and keeps the project"
stop_relay
start_relay "${RELAY#http://}"
as bob "$B" join "$INVITE" >"$T/rejoin.out"
grep -q "already linked" "$T/rejoin.out" || fail "$(cat "$T/rejoin.out")"
ok

# --- Phase 2: sessions through the real `seasoning mcp` and hooks -----------
#
# Each person gets a stand-in for the Claude Code process: a shell that
# starts `seasoning mcp` as its child (so the MCP server's parent PID names
# the runtime directory) and keeps its stdin on a fifo we write JSON-RPC to.

mcp_pids=""
start_session() { # PERSON SESSION_ID DIR
	who="$1"; sid="$2"; dir="$3"
	mkfifo "$T/$who.in"
	: >"$T/$who.out"
	SEASONING_CONFIG_DIR="$T/people/$who" CLAUDE_CODE_SESSION_ID="$sid" BIN="$BIN" IN="$T/$who.in" OUT="$T/$who.out" PIDF="$T/$who.pid" \
		sh -c 'echo $$ >"$PIDF"; "$BIN" mcp --plugin-proto 1 <"$IN" >"$OUT" 2>>"$OUT.err"; :' &
	mcp_pids="$mcp_pids $!"
	all_pids="$all_pids $!"
	i=0
	while [ ! -s "$T/$who.pid" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
	# Hold the fifo open for writing for the whole run.
	case "$who" in
		alice) exec 4>"$T/$who.in" ;;
		bob) exec 5>"$T/$who.in" ;;
	esac
	hook "$who" "$sid" "$dir" session-start '"source":"startup"' >"$T/$who.start"
	rpc "$who" 1 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}' >/dev/null
	send_line "$who" '{"jsonrpc":"2.0","method":"notifications/initialized"}'
}

send_line() {
	case "$1" in
		alice) printf '%s\n' "$2" >&4 ;;
		bob) printf '%s\n' "$2" >&5 ;;
	esac
}

# rpc PERSON ID JSON: send a request and wait for its response line.
rpc() {
	send_line "$1" "$3"
	i=0
	while ! grep -q "\"id\":$2[,}]" "$T/$1.out" && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
	grep "\"id\":$2[,}]" "$T/$1.out" | tail -1
}

tool() { # PERSON ID NAME ARGS_JSON
	rpc "$1" "$2" "{\"jsonrpc\":\"2.0\",\"id\":$2,\"method\":\"tools/call\",\"params\":{\"name\":\"$3\",\"arguments\":$4}}"
}

# hook PERSON SESSION_ID DIR EVENT EXTRA_JSON_FIELDS
hook() {
	printf '{"session_id":"%s","cwd":"%s",%s}' "$2" "$3" "$5" |
		SEASONING_CONFIG_DIR="$T/people/$1" CLAUDE_PID="$(cat "$T/$1.pid")" CLAUDE_CODE_SESSION_ATTENDED="${ATTENDED:-1}" \
			"$BIN" hook "$4" --plugin-proto 1
}

step "sessions start through the binary's hooks and MCP servers"
start_session alice e2e-alice-1 "$A"
start_session bob e2e-bob-1 "$B"
grep -q 'you are alice/' "$T/alice.start" || fail "alice's SessionStart: $(cat "$T/alice.start")"
ok

step "set_status renames, and peers see each other"
tool bob 2 set_status '{"label":"worker","status":"fixing the build"}' | grep -q 'You are now bob/worker' || fail "$(tail -2 "$T/bob.out")"
tool alice 2 set_status '{"label":"lead"}' >/dev/null
seen=""
for n in 3 4 5 6 7 8 9 10; do
	if tool alice "$n" peers '{}' | grep -q 'bob/worker'; then seen=1; break; fi
	sleep 0.5
done
[ -n "$seen" ] || fail "alice never saw bob/worker: $(tail -1 "$T/alice.out")"
ok

step "a direct message reaches bob's next tool call, once"
tool alice 11 send '{"to":["bob/worker"],"subject":"Build is red on main","body":"TestLogin fails since abc123.","reply_expected":true}' | grep -q 'bob/worker: live; queued' || fail "$(tail -1 "$T/alice.out")"
got=""
for i in 1 2 3 4 5 6 7 8 9 10; do
	out="$(hook bob e2e-bob-1 "$B" post-tool-use '"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"}')"
	if printf '%s' "$out" | grep -q 'Build is red on main'; then got=1; break; fi
	sleep 0.3
done
[ -n "$got" ] || fail "bob's PostToolUse never injected the message"
[ -z "$(hook bob e2e-bob-1 "$B" post-tool-use '"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{}')" ] || fail "message injected twice"
ok

step "a waiting idle session wakes with exit 2"
( set +e; hook bob e2e-bob-1 "$B" wait '"hook_event_name":"Stop"' >/dev/null 2>"$T/wait.err"; echo $? >"$T/wait.code" ) &
sleep 0.5
tool alice 12 send '{"to":["bob/worker"],"subject":"Are you on it?","body":"Ping.","reply_expected":true}' >/dev/null
i=0
while [ ! -s "$T/wait.code" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
[ "$(cat "$T/wait.code" 2>/dev/null)" = 2 ] || fail "waiter exit code: $(cat "$T/wait.code" 2>/dev/null || echo none)"
grep -q 'Are you on it?' "$T/wait.err" || fail "waiter output: $(cat "$T/wait.err")"
ok

step "an unattended waiter returns at once (claude -p must not block)"
ATTENDED=0 hook bob e2e-bob-1 "$B" wait '"hook_event_name":"Stop"' || fail "unattended waiter failed"
ok

step "closing an MCP server's stdin ends its session at the relay"
exec 4>&- 5>&-
for pid in $mcp_pids; do wait "$pid" 2>/dev/null || true; done
sleep 0.5
grep -q '"route":"DELETE /v1/projects/{p}/sessions/{s}","status":204' "$T/relay.log" || fail "no session end reached the relay"
ok

step "relay stops cleanly on SIGTERM"
stop_relay
ok

echo "e2e: all checks passed"
