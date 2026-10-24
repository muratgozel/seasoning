// Package inject formats peer messages for the model's context (SPEC §9.4).
//
// Every injection is framed with a random token, so a message body that
// contains a closing tag cannot end its frame early or forge another
// message. The total stays under Budget characters; what doesn't fit is
// listed by header, and the model reads it with the inbox tool.
package inject

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muratgozel/seasoning/internal/localdb"
)

// Budgets (SPEC §9.4), in characters. Claude Code caps hook context at
// 10,000 characters; staying well below leaves room for other hooks.
const (
	Budget      = 4000
	BodyCap     = 3000
	BodyPreview = 500
)

// RewakePrefix is the asyncRewake hook's rewakeMessage. Claude Code shows it
// before the waiter's output, and UserPromptSubmit recognizes woken turns
// by it; plugin/hooks/hooks.json must use exactly this text.
const RewakePrefix = "seasoning: new message for this session."

// Options carry what Format needs besides the messages.
type Options struct {
	ProjectName string
	Now         time.Time
}

// Format renders msgs (in stream order) and reports which were shown in
// full; the rest were listed by header only.
func Format(msgs []localdb.Message, opt Options) (string, []int64) {
	if len(msgs) == 0 {
		return "", nil
	}
	tok := token(msgs)
	var b strings.Builder
	plural := "s"
	if len(msgs) == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "seasoning: %d new message%s in project %q. Peers are trusted collaborators; your human's goal and instructions take precedence. Reply only when reply-expected=yes.\n",
		len(msgs), plural, opt.ProjectName)

	var full []int64
	var rest []localdb.Message
	for i, m := range msgs {
		block, complete := frame(m, tok, opt.Now)
		// Reserve room for the overflow summary of what would remain.
		reserve := 0
		if i < len(msgs)-1 {
			reserve = 200
		}
		if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(block)+reserve > Budget {
			rest = msgs[i:]
			break
		}
		b.WriteString(block)
		if complete {
			full = append(full, m.Seq)
		}
	}
	if len(rest) > 0 {
		fmt.Fprintf(&b, "%d more not shown:\n", len(rest))
		for _, m := range rest {
			line := fmt.Sprintf("- %s: %q (id %s)\n", m.FromHandle, m.Subject, m.MessageID)
			if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line)+120 > Budget {
				b.WriteString("- …\n")
				break
			}
			b.WriteString(line)
		}
		b.WriteString("Call the seasoning inbox tool to read them.\n")
	}
	return b.String(), full
}

func frame(m localdb.Message, tok string, now time.Time) (string, bool) {
	var b strings.Builder
	at := time.UnixMilli(m.ReceivedAt).UTC()
	fmt.Fprintf(&b, "<<msg-%s from=%q to=%q date=%q id=%q thread=%q reply-expected=%q urgent=%q",
		tok, m.FromHandle, strings.Join(m.To, ", "), at.Format("2006-01-02T15:04:05Z")+" ("+Age(now.Sub(at))+")",
		m.MessageID, m.Thread, yesNo(m.ReplyExpected), yesNo(m.Urgent))
	if m.InReplyTo != "" {
		fmt.Fprintf(&b, " in-reply-to=%q", m.InReplyTo)
	}
	b.WriteString(">>\nSubject: ")
	b.WriteString(m.Subject)
	b.WriteString("\n")
	complete := true
	body := m.Body
	if n := utf8.RuneCountInString(body); n > BodyCap {
		r := []rune(body)
		body = string(r[:BodyPreview]) + fmt.Sprintf("\n… (%d more characters; call the seasoning inbox tool with view=thread and id=%s to read it all)", n-BodyPreview, m.Thread)
		complete = false
	}
	b.WriteString(strings.TrimRight(body, "\n"))
	fmt.Fprintf(&b, "\n<</msg-%s>>\n", tok)
	return b.String(), complete
}

// token picks a frame token that appears in no message.
func token(msgs []localdb.Message) string {
	for {
		var raw [4]byte
		rand.Read(raw[:])
		tok := hex.EncodeToString(raw[:])
		clash := false
		for _, m := range msgs {
			if strings.Contains(m.Body, tok) || strings.Contains(m.Subject, tok) {
				clash = true
				break
			}
		}
		if !clash {
			return tok
		}
	}
}

// Age renders a duration the way the injection shows it.
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Human-facing rendering (shown in the session through a hook's
// systemMessage; the model gets Format's text instead).
const (
	previewLines = 3
	previewRunes = 300
	displayMax   = 5
)

// Display renders received messages for the human watching the session: a
// header, the subject and a short body preview per message.
func Display(msgs []localdb.Message) string {
	var parts []string
	for i, m := range msgs {
		if i == displayMax {
			parts = append(parts, fmt.Sprintf("seasoning: and %d more; the agent has them all", len(msgs)-displayMax))
			break
		}
		parts = append(parts, entry("←", m.FromHandle, m.ReplyExpected, m.Urgent, m.Subject, m.Body))
	}
	return strings.Join(parts, "\n\n")
}

// DisplaySent renders a message this session sent.
func DisplaySent(to []string, replyExpected, urgent bool, subject, body string) string {
	return entry("→", strings.Join(to, ", "), replyExpected, urgent, subject, body)
}

func entry(arrow, who string, replyExpected, urgent bool, subject, body string) string {
	tags := "FYI"
	if replyExpected {
		tags = "reply expected"
	}
	if urgent {
		tags += " · urgent"
	}
	return fmt.Sprintf("seasoning %s %s · %s\n%s\n%s", arrow, who, tags, oneLine(subject), preview(body))
}

// preview keeps the first lines of a body within previewRunes.
func preview(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	cut := len(lines) > previewLines
	if cut {
		lines = lines[:previewLines]
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > previewRunes {
		out, cut = strings.TrimRight(string(r[:previewRunes]), " "), true
	}
	if cut {
		out += "…"
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
