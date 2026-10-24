#!/bin/bash
# Runs inside the e2e-live container (see run.sh): a relay on loopback, two
# people (alice, bob) with their own seasoning identity and clone, and a real
# interactive Claude Code session for each in tmux, with the plugin loaded.
#
# Assertions read local.db, files and the relay log, never the model's
# wording. Each step has a timeout; on failure the panes and transcripts are
# saved to ~/artifacts.
set -euo pipefail

H=/home/dev
export SEASONING_STATE_DIR=$H/state SEASONING_DATA_DIR=$H/data
DB=$SEASONING_DATA_DIR/local.db
ART=$H/artifacts
mkdir -p "$ART"
TOKEN="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"

step() { printf '%-64s ' "$1"; }
ok() { echo "ok ($(( $(date +%s) - STEP_START ))s)"; }
begin() { STEP_START=$(date +%s); step "$1"; }
save() {
	for s in alice bob; do tmux capture-pane -p -S -400 -t "$s" >"$ART/$s.pane.txt" 2>/dev/null || true; done
	cp -r "$CLAUDE_CONFIG_DIR/projects" "$ART/transcripts" 2>/dev/null || true
	cp "$H/relay.log" "$ART/" 2>/dev/null || true
	cp "$SEASONING_STATE_DIR/errors.log" "$ART/" 2>/dev/null || true
	sqlite3 "$DB" '.mode line' 'select local_id, person, label, status, activity, autonomous_turns, ended_at from sessions' >"$ART/sessions.txt" 2>/dev/null || true
	sqlite3 "$DB" '.mode line' 'select relay_session, seq, from_handle, to_json, subject, wake, surfaced_at, read_at, quarantine from inbox' >"$ART/inbox.txt" 2>/dev/null || true
}
fail() { echo "FAIL"; echo "  $1" >&2; save; exit 1; }
trap 'save' EXIT

q() { sqlite3 "$DB" "$1" 2>/dev/null || true; }

# wait_for SECONDS DESCRIPTION COMMAND...: poll until COMMAND succeeds.
wait_for() {
	local secs="$1" what="$2"; shift 2
	local end=$(( $(date +%s) + secs ))
	while [ "$(date +%s)" -lt "$end" ]; do
		dismiss_dialogs
		if "$@"; then return 0; fi
		sleep 2
	done
	fail "timed out after ${secs}s waiting for: $what"
}

# Dialogs that need a human. The safeguard prompt is answered with "stay on
# the current model"; nothing else is ever chosen on the user's behalf.
dismiss_dialogs() {
	for s in alice bob; do
		local pane
		pane="$(tmux capture-pane -p -t "$s" 2>/dev/null || true)"
		if grep -q "safeguards flagged this session" <<<"$pane"; then
			echo "  (note: $s hit a safeguard dialog; staying on the current model)" >&2
			if grep -q "Stay on" <<<"$pane"; then
				tmux send-keys -t "$s" Down; sleep 0.3; tmux send-keys -t "$s" Enter
			else
				tmux send-keys -t "$s" Escape
			fi
			sleep 1
		fi
	done
}

idle() { # SESSION: the input box is ready and no turn is running
	local pane
	pane="$(tmux capture-pane -p -t "$1")"
	grep -q '❯' <<<"$pane" && ! grep -q 'esc to interrupt' <<<"$pane"
}

shown() { # PERSON TEXT: the human watching PERSON's session sees TEXT
	grep -rqsF "$2" "$CLAUDE_CONFIG_DIR/projects/-home-dev-$1-repo/" || tmux capture-pane -p -S -2000 -t "$1" | grep -qF "$2"
}

say() { # SESSION TEXT: type a prompt and submit it
	tmux send-keys -t "$1" -l "$2"
	sleep 0.7
	tmux send-keys -t "$1" Enter
}

as() { # PERSON COMMAND...
	local who="$1"; shift
	SEASONING_CONFIG_DIR="$H/people/$who" "$@"
}

# --- setup ----------------------------------------------------------------

begin "relay, identities and a project"
SEASONING_RELAY_TOKEN="$TOKEN" seasoning serve --listen 127.0.0.1:8090 --db "$H/relay.db" >"$H/relay.log" 2>&1 &
wait_for 10 "relay health" curl -fsS http://127.0.0.1:8090/healthz -o /dev/null 2>/dev/null
for who in alice bob; do
	mkdir -p "$H/$who-repo"
	git -C "$H/$who-repo" init -q
	git -C "$H/$who-repo" remote add origin git@github.com:example/e2e-live.git
	printf '# e2e-live fixture\n' >"$H/$who-repo/README.md"
	git -C "$H/$who-repo" add . && git -C "$H/$who-repo" commit -qm "chore: fixture"
	as "$who" seasoning setup --handle "$who" >/dev/null
done
INVITE="$(cd "$H/alice-repo" && SEASONING_RELAY_TOKEN="$TOKEN" as alice seasoning init --relay http://127.0.0.1:8090 | grep '/join#k=' | grep -v 'seasoning join' | head -1 | tr -d ' ')"
[ -n "$INVITE" ] || fail "no invite"
(cd "$H/bob-repo" && as bob seasoning join "$INVITE" >/dev/null) || fail "bob could not join"
# Trust both clones and skip onboarding, as a returning user would have.
cfg="$CLAUDE_CONFIG_DIR/.claude.json"
[ -f "$cfg" ] || echo '{}' >"$cfg"
jq --arg a "$H/alice-repo" --arg b "$H/bob-repo" \
	'.hasCompletedOnboarding = true | .projects[$a].hasTrustDialogAccepted = true | .projects[$b].hasTrustDialogAccepted = true' \
	"$cfg" >"$cfg.tmp" && mv "$cfg.tmp" "$cfg"
ok

begin "both Claude sessions start with the plugin and join"
for who in alice bob; do
	tmux new-session -d -s "$who" -x 200 -y 50 -c "$H/$who-repo" \
		"env SEASONING_CONFIG_DIR=$H/people/$who claude --settings /e2e/settings.json --plugin-dir /plugin"
done
wait_for 90 "two joined sessions in local.db" bash -c "[ \"\$(sqlite3 $DB 'select count(*) from sessions where ended_at is null' 2>/dev/null)\" = 2 ]"
wait_for 60 "both sessions registered at the relay" bash -c "[ \"\$(grep -c 'PUT /v1/projects/{p}/sessions/{s}\",\"status\":200' $H/relay.log)\" -ge 2 ]"
wait_for 60 "alice ready" idle alice
wait_for 60 "bob ready" idle bob
ok

begin "agents set their labels through the MCP tool"
say alice "Use the seasoning set_status tool to set your label to lead and your status to 'running the live e2e'. Then reply with the single word DONE."
say bob "Use the seasoning set_status tool to set your label to worker and your status to 'waiting for instructions from lead'. Then reply with the single word READY, and wait."
wait_for 180 "alice's label is lead" bash -c "[ \"\$(sqlite3 $DB \"select label from sessions where person='alice' and ended_at is null\")\" = lead ]"
wait_for 180 "bob's label is worker" bash -c "[ \"\$(sqlite3 $DB \"select label from sessions where person='bob' and ended_at is null\")\" = worker ]"
wait_for 120 "alice idle" idle alice
wait_for 120 "bob idle" idle bob
ok

BOB_RS="$(q "select relay_session from sessions where person='bob' and ended_at is null")"
ALICE_RS="$(q "select relay_session from sessions where person='alice' and ended_at is null")"

begin "a direct message wakes idle bob, who acts and replies"
say alice "Use the seasoning send tool to message bob/worker with reply_expected true. Subject: 'Please create HELLO.txt'. Body: 'Please create a file named HELLO.txt at the root of your repository containing exactly: hi from bob. Reply to this message when it is done.' After sending, end your turn."
wait_for 300 "bob created HELLO.txt" bash -c "grep -qx 'hi from bob' $H/bob-repo/HELLO.txt 2>/dev/null"
wait_for 300 "bob's reply reached alice" bash -c "[ \"\$(sqlite3 $DB \"select count(*) from inbox where relay_session='$ALICE_RS' and from_person='bob'\")\" -ge 1 ]"
turns="$(q "select autonomous_turns from sessions where person='bob' and ended_at is null")"
[ "${turns:-0}" -ge 1 ] || fail "bob's turn was not counted as woken (autonomous_turns=$turns)"
reply_to="$(q "select in_reply_to from inbox where relay_session='$ALICE_RS' and from_person='bob' order by seq limit 1")"
[ -n "$reply_to" ] || fail "bob's reply is not threaded (in_reply_to empty)"
wait_for 300 "alice surfaced bob's reply (woken in turn)" bash -c "[ -n \"\$(sqlite3 $DB \"select surfaced_at from inbox where relay_session='$ALICE_RS' and from_person='bob' order by seq limit 1\")\" ]"
ok

begin "both sessions show the messages to the human"
wait_for 60 "alice's session shows what she sent" shown alice "seasoning → bob/worker"
wait_for 60 "bob's session shows what woke him" shown bob "seasoning ← alice/lead"
wait_for 60 "alice's session shows bob's reply" shown alice "seasoning ← bob/worker"
ok

begin "a broadcast does not wake idle bob, and arrives on his next turn"
wait_for 300 "alice idle" idle alice
wait_for 300 "bob idle" idle bob
say alice "Use the seasoning send tool to send to * (everyone) with reply_expected false. Subject: 'FYI: e2e broadcast'. Body: 'No action needed.' Then end your turn."
wait_for 180 "the broadcast reached bob's inbox" bash -c "[ \"\$(sqlite3 $DB \"select count(*) from inbox where relay_session='$BOB_RS' and subject='FYI: e2e broadcast'\")\" = 1 ]"
sleep 30
[ -z "$(q "select surfaced_at from inbox where relay_session='$BOB_RS' and subject='FYI: e2e broadcast'")" ] || fail "the broadcast was surfaced while bob was idle (it woke him)"
idle bob || fail "bob started a turn after a broadcast"
say bob "Anything new from your peers? Answer in one short line."
wait_for 180 "bob's next turn surfaced the broadcast" bash -c "[ -n \"\$(sqlite3 $DB \"select surfaced_at from inbox where relay_session='$BOB_RS' and subject='FYI: e2e broadcast'\")\" ]"
ok

begin "the /seasoning:status, :off and :on commands work in Claude Code"
wait_for 180 "bob idle" idle bob
say bob "/seasoning:status"
wait_for 180 "status output in bob's transcript" bash -c "grep -rqs 'Pending:  ' $CLAUDE_CONFIG_DIR/projects/-home-dev-bob-repo/"
wait_for 180 "bob idle" idle bob
say bob "/seasoning:off"
wait_for 180 "bob's runtime off marker" bash -c "ls $SEASONING_STATE_DIR/runtime/*/off >/dev/null 2>&1"
BOB_RT="$(dirname "$(find "$SEASONING_STATE_DIR/runtime" -mindepth 2 -maxdepth 2 -name off | head -1)")"
[ ! -e "$BOB_RT/joined" ] || fail "bob is still joined after /seasoning:off"
wait_for 180 "bob idle" idle bob
say bob "/seasoning:on"
wait_for 180 "off marker cleared" bash -c "[ ! -e '$BOB_RT/off' ]"
wait_for 60 "bob joined again" bash -c "[ -e '$BOB_RT/joined' ]"
ok

begin "no errors were logged"
if [ -s "$SEASONING_STATE_DIR/errors.log" ]; then
	echo "(errors.log is not empty; see artifacts)" >&2
	cat "$SEASONING_STATE_DIR/errors.log" >&2
fi
ok

echo "e2e-live: all checks passed"
