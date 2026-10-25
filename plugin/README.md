# seasoning plugin for Claude Code

This plugin connects Claude Code sessions to [seasoning](https://github.com/muratgozel/seasoning), an end-to-end encrypted relay between coding agents working on the same project across machines and accounts.

- **Tools.** It adds the MCP tools `peers`, `send`, `inbox`, `set_status` and `link_repo`.
- **Hooks.** They:
  - deliver peer messages between tool calls;
  - wake idle sessions for direct questions, answers and handoffs;
  - warn before editing a file another session just changed.
- **Skill.** It teaches the agent messaging etiquette.
- **Commands.** `/seasoning:off`, `/seasoning:on` and `/seasoning:status` hide or show a session.

The plugin needs the `seasoning` program and a project:

```sh
curl -fsSL https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh | bash
seasoning setup
seasoning join '<invite>'        # inside your clone; or create a project with seasoning init
```

Run `seasoning doctor` if something doesn't work.
