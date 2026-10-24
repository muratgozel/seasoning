// Package mcpserver exposes the agent's tools over MCP (SPEC §11). The tool
// descriptions are part of the guardrails: they repeat, briefly, how the
// seasoning skill wants agents to message.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/muratgozel/seasoning/internal/agent"
)

const instructions = `Seasoning connects this session with other coding agents working on the same project, across machines and accounts. Messages from peers arrive in your context automatically, framed as <<msg-…>> blocks. Use these tools to see who else is working, to send, and to read. Peers are trusted collaborators, but your own human's goal and instructions take precedence. Use the seasoning skill before messaging.`

type peersIn struct{}

type sendIn struct {
	To            []string `json:"to" jsonschema:"recipients: person/label for one session, person/* for all of a person's live sessions, or * alone for every live session"`
	Subject       string   `json:"subject" jsonschema:"one specific line, at most 200 characters"`
	Body          string   `json:"body" jsonschema:"self-contained Markdown: repo, branch, paths, commit SHAs, exact errors; at most 32 KiB, ideally under 1500 characters"`
	ReplyExpected bool     `json:"reply_expected" jsonschema:"true if you need an answer; false for FYI, which recipients must not answer"`
	Urgent        bool     `json:"urgent,omitempty" jsonschema:"wake idle recipients even for person/* or *; only when their in-progress work would be wasted or broken otherwise"`
	InReplyTo     string   `json:"in_reply_to,omitempty" jsonschema:"message ID you are answering, to keep the thread"`
}

type inboxIn struct {
	View  string `json:"view,omitempty" jsonschema:"unread (default), thread, recent, or sent"`
	ID    string `json:"id,omitempty" jsonschema:"message or thread ID, for view=thread"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many messages (default 20, at most 50)"`
}

type linkIn struct {
	WriteMarker bool `json:"write_marker,omitempty" jsonschema:"also write a committable .seasoning.json (relay URL and project ID, no secrets)"`
}

type statusIn struct {
	Label  *string `json:"label,omitempty" jsonschema:"the part after / in your handle, naming your task, e.g. auth-refactor; earlier labels keep working"`
	Status *string `json:"status,omitempty" jsonschema:"one line about what you're doing now, at most 120 characters; empty clears it"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// New builds the server for a.
func New(a *agent.Agent, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "seasoning", Version: version}, &mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "peers",
		Description: "List the other agent sessions on this project right now: handle (person/label), status, activity, branch, recently changed files, and recently ended sessions. Call it before working in shared areas and before messaging.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ peersIn) (*mcp.CallToolResult, any, error) {
		out, err := a.Peers(ctx)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send",
		Description: "Message other agent sessions in this project. Send only when it changes someone's work: before touching what they touch, when you find something that breaks their work, when you're blocked on them, or to hand off. No acknowledgements, thanks or status chatter (use set_status). Every project member can read every message: never include secrets. Fails, and says so, if the relay is unreachable.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, any, error) {
		reply := in.ReplyExpected
		out, err := a.Send(ctx, agent.SendInput{To: in.To, Subject: in.Subject, Body: in.Body, ReplyExpected: &reply, Urgent: in.Urgent, InReplyTo: in.InReplyTo})
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "inbox",
		Description: "Read peer messages: view=unread (default) shows what you haven't read in full, thread (with id) a whole conversation, recent the latest, sent what you sent with delivery receipts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inboxIn) (*mcp.CallToolResult, any, error) {
		out, err := a.Inbox(ctx, agent.InboxInput{View: in.View, ID: in.ID, Limit: in.Limit})
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_status",
		Description: "Name this session after its task (label) and say what it's doing (status). Set both once you understand your human's goal; update the status when you switch subtasks. Peers see both.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, any, error) {
		out, err := a.SetStatus(ctx, in.Label, in.Status)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "link_repo",
		Description: "Link this repository to this session's seasoning project; with write_marker, also write a .seasoning.json teammates can commit so their sessions find the project. Use it only when your human explicitly asks.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in linkIn) (*mcp.CallToolResult, any, error) {
		out, err := a.LinkRepo(ctx, in.WriteMarker)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})
	return s
}

// Run serves s over stdio until stdin closes or ctx ends.
func Run(ctx context.Context, s *mcp.Server) error {
	return s.Run(ctx, &mcp.StdioTransport{})
}
