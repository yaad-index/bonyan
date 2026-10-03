package memory

import (
	"context"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
)

// Messages returns a session's events as conversation for a run's history
// (agent.Agent.History), oldest first: an event that came from model output
// as an assistant message, any other as a user message. Every part is
// untrusted and keeps its memory source, whatever the policy decided as it was
// read: a run's history holds no trusted text, and the run classifies it
// again where it enters (ADR 0001 §3), so a policy that trusts it trusts it
// there.
func Messages(ctx context.Context, s *Store, subject, session string) ([]model.Message, error) {
	events, err := s.History(ctx, subject, session)
	if err != nil {
		return nil, err
	}
	out := make([]model.Message, 0, len(events))
	for _, e := range events {
		var from content.Provenance
		var text string
		switch v := e.(type) {
		case content.Untrusted:
			from, text = v.Provenance(), v.Raw()
		case content.Trusted:
			from, text = v.Provenance(), v.String()
		}
		role := model.RoleUser
		if from.Origin == content.KindModel {
			role = model.RoleAssistant
		}
		out = append(out, model.Message{Role: role, Parts: []content.Text{content.From(from, text)}})
	}
	return out, nil
}
