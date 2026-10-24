# Live end-to-end sandbox (SPEC §20.3): Ubuntu 26.04 with Claude Code, tmux
# to drive interactive sessions, and sqlite3 for assertions. The seasoning
# binary, the plugin and the scenario are mounted at run time.
FROM ubuntu:26.04

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git tmux jq sqlite3 procps less \
 && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash dev
USER dev
WORKDIR /home/dev

ARG CLAUDE_VERSION=2.1.310
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- "${CLAUDE_VERSION}"

ENV PATH=/home/dev/.local/bin:$PATH \
    CLAUDE_CONFIG_DIR=/home/dev/.claude-cfg \
    TERM=xterm-256color

# Created as dev so the login volume mounted here inherits dev's ownership.
RUN mkdir -p /home/dev/.claude-cfg \
 && git config --global user.name "E2E Dev" \
 && git config --global user.email "e2e@example.com" \
 && git config --global init.defaultBranch main

CMD ["sleep", "infinity"]
