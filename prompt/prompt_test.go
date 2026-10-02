package prompt_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/prompt"
)

func TestAPromptsRef(t *testing.T) {
	p := prompt.Prompt{Name: "helper", Version: "3", Text: "be brief"}
	require.NoError(t, p.Validate())
	assert.Equal(t, "helper@3", p.ID())
	assert.Equal(t, "be brief", p.Instruction().String())
	r := p.Ref()
	assert.Equal(t, "helper@3", r.ID)
	// sha256 of "be brief", computed outside Go.
	assert.Equal(t, "sha256:b0d336336bae9756708102764ccc977778d8c408df38d7050d69f9e4d22c9a43", r.Hash)
	assert.NotContains(t, r.ID+r.Hash, "be brief")

	edited := p
	edited.Text = "be brief!"
	assert.NotEqual(t, r.Hash, edited.Ref().Hash, "an edit changes the hash")
	assert.Equal(t, r.Hash, prompt.Prompt{Name: "other", Version: "9", Text: "be brief"}.Ref().Hash, "the hash is of the text")

	u := prompt.Unversioned(content.Instruction("be brief"))
	assert.Empty(t, u.ID)
	assert.Equal(t, r.Hash, u.Hash)

	for _, bad := range []prompt.Prompt{{Version: "1", Text: "x"}, {Name: "a", Text: "x"}, {Name: "a", Version: "1"}} {
		require.Error(t, bad.Validate(), "%+v", bad)
	}
}

func TestARefTravelsInTheContext(t *testing.T) {
	_, ok := prompt.RefOf(context.Background())
	assert.False(t, ok)
	want := prompt.Ref{ID: "helper@3", Hash: "sha256:00"}
	got, ok := prompt.RefOf(prompt.WithRef(context.Background(), want))
	require.True(t, ok)
	assert.Equal(t, want, got)
}
