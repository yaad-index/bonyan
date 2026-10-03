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

// The part the recording marks as the run's input is the run's message,
// wherever it is in the first request and whatever ends that request; the
// marker wins over the layout an older recording is read by.
func TestTheMarkedPartIsTheRunsMessage(t *testing.T) {
	from := &record.Provenance{Kind: content.KindUser, ID: "m"}
	marked := record.Part{Input: true, Section: agent.SectionUserMessage, Items: []record.Part{{Text: "MARKED-3b2", Provenance: from}}}
	byLayout := record.Part{Section: agent.SectionUserMessage, Items: []record.Part{{Text: "BY-LAYOUT-3b2", Provenance: from}}}
	run := func(msgs ...record.Message) record.Run {
		return record.Run{Calls: []record.Call{
			{Kind: record.KindClassify},
			{Kind: record.KindChat},
			{Kind: record.KindChat, Request: &record.Request{Messages: msgs}},
		}}
	}
	for name, tc := range map[string]struct {
		run  record.Run
		want string
	}{
		"not in the last message": {run(
			record.Message{Role: model.RoleUser, Parts: []record.Part{marked}},
			record.Message{Role: model.RoleAssistant, Parts: []record.Part{{Trusted: true, Text: "calling"}}},
		), "MARKED-3b2"},
		"over the layout": {run(
			record.Message{Role: model.RoleUser, Parts: []record.Part{marked}},
			record.Message{Role: model.RoleUser, Parts: []record.Part{byLayout}},
		), "MARKED-3b2"},
		"trusted, with no section": {run(
			record.Message{Role: model.RoleUser, Parts: []record.Part{{Input: true, Trusted: true, Text: "TRUSTED-3b2", Provenance: from}}},
		), "TRUSTED-3b2"},
		"unmarked, by the layout": {run(
			record.Message{Role: model.RoleUser, Parts: []record.Part{byLayout}},
		), "BY-LAYOUT-3b2"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := runInput(tc.run)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Raw())
			assert.Equal(t, content.Provenance{Kind: content.KindUser, ID: "m"}, got.Provenance())
		})
	}
}

// A marked part that does not hold one message with a source is refused,
// even when the layout would find another: a bad marker is a bad recording,
// not an older one.
func TestABadMarkerIsRefused(t *testing.T) {
	from := &record.Provenance{Kind: content.KindUser, ID: "m"}
	it := record.Part{Text: "x", Provenance: from}
	byLayout := record.Part{Section: agent.SectionUserMessage, Items: []record.Part{it}}
	for name, bad := range map[string]record.Part{
		"two items":          {Input: true, Section: agent.SectionUserMessage, Items: []record.Part{it, it}},
		"no item":            {Input: true, Section: agent.SectionUserMessage},
		"an item, no source": {Input: true, Section: agent.SectionUserMessage, Items: []record.Part{{Text: "x"}}},
		"an excluded item":   {Input: true, Section: agent.SectionUserMessage, Items: []record.Part{{Excluded: true, Provenance: from}}},
		"bare, no source":    {Input: true, Trusted: true, Text: "x"},
		"bare, excluded":     {Input: true, Trusted: true, Excluded: true, Provenance: from},
	} {
		run := record.Run{Calls: []record.Call{{Kind: record.KindChat, Request: &record.Request{Messages: []record.Message{
			{Role: model.RoleUser, Parts: []record.Part{bad}},
			{Role: model.RoleUser, Parts: []record.Part{byLayout}},
		}}}}}
		_, err := runInput(run)
		assert.Error(t, err, name)
	}
}
