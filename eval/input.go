package eval

import (
	"errors"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
)

// runInput is the message a recorded run started from, as the text it was,
// with its source. Its source kind cannot tell it apart, since earlier user
// messages are user input too and a run may start from any kind; its section
// does: the loop puts it in a section labelled agent.SectionUserMessage, and
// context assembly puts this run's turns last, its message first
// (assemble.Build). It is therefore the item of that section in the last
// message of the run's first chat request, or, when the trust policy declared
// it trusted and it has no section, that message's part with a source. A
// recording laid out any other way is refused rather than guessed at.
// TestTheRunsMessageIsFound pins that layout.
func runInput(run record.Run) (content.Untrusted, error) {
	for _, c := range run.Calls {
		if c.Kind != record.KindChat || c.Request == nil {
			continue
		}
		msgs := c.Request.Messages
		if len(msgs) == 0 || msgs[len(msgs)-1].Role != model.RoleUser {
			break
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
		break
	}
	return content.Untrusted{}, errors.New("eval: the recorded run holds no user message to run again")
}
