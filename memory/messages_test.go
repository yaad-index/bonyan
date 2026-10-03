package memory_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/trust"
)

// A session comes back as conversation: model output as the assistant's
// turns, the rest as the user's, every part untrusted memory with its origin,
// even when the policy trusts it.
func TestMessagesAreTheSessionAsConversation(t *testing.T) {
	for _, v := range []trust.Verdict{trust.Untrusted, trust.Trusted} {
		t.Run(v.String(), func(t *testing.T) {
			p := &policy{verdict: v}
			c := &clock{t: start}
			s := newStore(t, inmem.New("test"), p, c)
			for _, e := range []struct {
				origin content.Kind
				text   string
			}{{content.KindUser, "hi"}, {content.KindModel, "hello"}, {content.KindFetched, "a mail"}} {
				require.NoError(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: e.origin}, e.text))
				c.t = c.t.Add(time.Second)
			}
			history, err := s.History(ctx, "ana", "s1")
			require.NoError(t, err)
			require.Len(t, history, 3)
			assert.Equal(t, v == trust.Trusted, history[1].Trusted(), "the store's own reading")

			got, err := memory.Messages(ctx, s, "ana", "s1")
			require.NoError(t, err)
			require.Len(t, got, 3)
			for i, want := range []struct {
				role   model.Role
				origin content.Kind
				text   string
			}{{model.RoleUser, content.KindUser, "hi"}, {model.RoleAssistant, content.KindModel, "hello"}, {model.RoleUser, content.KindFetched, "a mail"}} {
				require.Len(t, got[i].Parts, 1)
				u, ok := got[i].Parts[0].(content.Untrusted)
				require.True(t, ok, "untrusted")
				assert.Equal(t, want.role, got[i].Role)
				assert.Equal(t, want.text, u.Raw())
				assert.Equal(t, content.KindMemory, u.Provenance().Kind)
				assert.Equal(t, want.origin, u.Provenance().Origin)
				assert.NotEmpty(t, u.Provenance().ID)
			}
		})
	}
	_, err := memory.Messages(ctx, newStore(t, inmem.New("test"), &policy{}, &clock{t: start}), "ana", "")
	assert.Error(t, err, "no session")
}
