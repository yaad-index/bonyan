package eval

import (
	"errors"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
)

// runInput is the message a recorded run started from, as the text it was,
// with its source. The recording marks the part that holds it (record.Part
// Input): the "user message" section the loop put it in, or the part itself
// when the trust policy declared it trusted. A marked part that does not hold
// one message with a source is refused.
//
// A recording without the marker, made before it existed or with hooks that
// moved the message, is read by where the message is. Its source kind cannot
// tell it apart, since earlier user messages are user input too and a run
// may start from any kind; its section does: the loop puts it in a section
// labelled agent.SectionUserMessage, and context assembly puts this run's
// turns last, its message first (assemble.Build). It is therefore the item of
// that section in the last message of the run's first chat request, or, when
// the trust policy declared it trusted and it has no section, that message's
// part with a source. A recording laid out any other way is refused rather
// than guessed at. TestTheRunsMessageIsFound pins that layout.
func runInput(run record.Run) (content.Untrusted, error) {
	req := firstChat(run)
	if req == nil {
		return content.Untrusted{}, errNoInput
	}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Input {
				return marked(p)
			}
		}
	}
	msgs := req.Messages
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != model.RoleUser {
		return content.Untrusted{}, errNoInput
	}
	for _, p := range msgs[len(msgs)-1].Parts {
		it := p
		switch {
		case p.Section == agent.SectionUserMessage && len(p.Items) == 1:
			it = p.Items[0]
		case p.Trusted && p.Section == "":
		default:
			continue
		}
		if it.Provenance == nil || it.Excluded {
			continue
		}
		return content.From(content.Provenance{Kind: it.Provenance.Kind, ID: it.Provenance.ID}, it.Text), nil
	}
	return content.Untrusted{}, errNoInput
}

var errNoInput = errors.New("eval: the recorded run holds no user message to run again")

// firstChat is the request of the run's first chat call that recorded one,
// or nil.
func firstChat(run record.Run) *record.Request {
	for _, c := range run.Calls {
		if c.Kind == record.KindChat && c.Request != nil {
			return c.Request
		}
	}
	return nil
}

// marked is the message the part marked as the run's input holds.
func marked(p record.Part) (content.Untrusted, error) {
	it := p
	if p.Section != "" {
		if len(p.Items) != 1 {
			return content.Untrusted{}, errors.New("eval: the run's marked input does not hold one message")
		}
		it = p.Items[0]
	}
	if it.Provenance == nil || it.Excluded {
		return content.Untrusted{}, errors.New("eval: the run's marked input has no source or no text")
	}
	return content.From(content.Provenance{Kind: it.Provenance.Kind, ID: it.Provenance.ID}, it.Text), nil
}
