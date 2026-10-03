package eval

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
)

// A recording whose first request does not end in the run's message, in its
// own section or trusted, is refused, never answered with another item.
func TestARecordingLaidOutOtherwiseIsRefused(t *testing.T) {
	part := func(section string, trusted bool) record.Part {
		it := record.Part{Trusted: trusted, Text: "x", Provenance: &record.Provenance{Kind: content.KindUser, ID: "m"}}
		if section == "" {
			return it
		}
		return record.Part{Section: section, Items: []record.Part{it}}
	}
	run := func(m record.Message) record.Run {
		return record.Run{Calls: []record.Call{{Kind: record.KindChat, Request: &record.Request{Messages: []record.Message{
			{Role: model.RoleSystem, Parts: []record.Part{{Trusted: true, Text: "answer"}}}, m,
		}}}}}
	}
	got, err := runInput(run(record.Message{Role: model.RoleUser, Parts: []record.Part{part(agent.SectionUserMessage, false)}}))
	require.NoError(t, err, "the layout the loop writes")
	assert.Equal(t, "x", got.Raw())

	for name, m := range map[string]record.Message{
		"another section":        {Role: model.RoleUser, Parts: []record.Part{part("history", false)}},
		"an untrusted bare part": {Role: model.RoleUser, Parts: []record.Part{part("", false)}},
		"not a user message":     {Role: model.RoleAssistant, Parts: []record.Part{part(agent.SectionUserMessage, false)}},
		"an excluded part":       {Role: model.RoleUser, Parts: []record.Part{{Section: agent.SectionUserMessage, Items: []record.Part{{Excluded: true, Provenance: &record.Provenance{Kind: content.KindUser}}}}}},
	} {
		_, err := runInput(run(m))
		assert.Error(t, err, name)
	}
}
