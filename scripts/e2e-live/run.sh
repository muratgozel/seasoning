#!/bin/sh
# Live end-to-end test with real Claude Code sessions (SPEC §20.3). Local and
# opt-in: it spends model turns on the account logged into the Docker volume.
#
#   make e2e-live
#   E2E_CLAUDE_VOLUME=<volume> make e2e-live   use an existing login volume
#   E2E_KEEP=1 make e2e-live                    keep the container afterwards
#
# One-time login (credentials stay in the volume, never in this repo):
#   docker run -it --rm -v seasoning-e2e-claude:/home/dev/.claude-cfg seasoning-e2e:live claude
#   then sign in and type /exit.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VOLUME="${E2E_CLAUDE_VOLUME:-seasoning-e2e-claude}"
NAME="seasoning-e2e-live"
ARTIFACTS="$ROOT/bin/e2e-live"

GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOTOOLCHAIN
arch="$(go env GOARCH)"
echo "e2e-live: building seasoning for linux/$arch"
(cd "$ROOT" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -o bin/seasoning-linux ./cmd/seasoning)
docker build -q -t seasoning-e2e:live "$ROOT/scripts/e2e-live" >/dev/null

if ! docker run --rm -v "$VOLUME:/home/dev/.claude-cfg" seasoning-e2e:live test -s /home/dev/.claude-cfg/.credentials.json; then
	echo "e2e-live: no Claude login in volume $VOLUME. Log in once with:" >&2
	echo "  docker run -it --rm -v $VOLUME:/home/dev/.claude-cfg seasoning-e2e:live claude" >&2
	exit 1
fi

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" \
	-v "$VOLUME:/home/dev/.claude-cfg" \
	-v "$ROOT/bin/seasoning-linux:/home/dev/.local/bin/seasoning:ro" \
	-v "$ROOT/plugin:/plugin:ro" \
	-v "$ROOT/scripts/e2e-live:/e2e:ro" \
	seasoning-e2e:live >/dev/null

status=0
docker exec "$NAME" bash /e2e/scenario.sh || status=$?

rm -rf "$ARTIFACTS" && mkdir -p "$ARTIFACTS"
docker cp "$NAME:/home/dev/artifacts/." "$ARTIFACTS/" >/dev/null 2>&1 || true
if [ "$status" -ne 0 ] || [ "${E2E_KEEP:-}" = 1 ]; then
	echo "e2e-live: container $NAME kept; artifacts in $ARTIFACTS"
	echo "  inspect: docker exec -it $NAME tmux attach -t alice"
else
	docker rm -f "$NAME" >/dev/null
	echo "e2e-live: artifacts in $ARTIFACTS"
fi
exit "$status"
