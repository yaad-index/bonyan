package tokenize_test

import (
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

func msg(role model.Role, parts ...content.Text) model.Message {
	return model.Message{Role: role, Parts: parts}
}

// Multi-byte text is counted by bytes, not characters: a character-based
// bound would count this text at a third or a quarter of its bytes.
func TestMultiByteTextIsNotUndercounted(t *testing.T) {
	for _, s := range []string{"سلام دنیا", "日本語のテキスト", "emoji 🙂🙂🙂", "Grüße"} {
		req := model.ChatRequest{Messages: []model.Message{
			msg(model.RoleUser, content.From(content.Provenance{Kind: content.KindUser}, s)),
		}}
		n, err := tokenize.ByteBound{PerMessage: 1}.Count(req)
		require.NoError(t, err)
		assert.Equal(t, int64(1+len("user")+len(s)), n, s)
		assert.Greater(t, n, int64(utf8.RuneCountInString(s)+1+len("user")), "%q has more bytes than characters", s)
	}
}

func TestEveryPartOfTheRequestIsCounted(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	params := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	req := model.ChatRequest{
		Messages: []model.Message{
			msg(model.RoleSystem, content.Instruction("be brief")),
			msg(model.RoleUser, content.Instruction("a"), content.From(content.Provenance{Kind: content.KindFetched}, "bc")),
			model.ToolResult("c1", "result"),
		},
		Tools:  []model.ToolDef{{Name: "search", Description: "find things", Parameters: params}},
		Schema: schema,
	}
	const per = 5
	n, err := tokenize.ByteBound{PerMessage: per}.Count(req)
	require.NoError(t, err)

	want := int64(per + len("system") + len("be brief") +
		per + len("user") + len("a") + len("bc") +
		per + len("tool") + len("result") +
		per + len("search") + len("find things") + len(params) +
		len(schema))
	assert.Equal(t, want, n)
}

func TestDefaultAllowance(t *testing.T) {
	req := model.ChatRequest{Messages: []model.Message{msg(model.RoleUser)}}
	n, err := tokenize.ByteBound{}.Count(req)
	require.NoError(t, err)
	assert.Equal(t, int64(tokenize.DefaultPerMessage+len("user")), n)
}

func TestASectionCountsAsItRenders(t *testing.T) {
	s := content.NewSection("material", content.From(content.Provenance{Kind: content.KindFetched, ID: "d"}, "page"))
	n, err := tokenize.ByteBound{PerMessage: 1}.Count(model.ChatRequest{Messages: []model.Message{msg(model.RoleUser, s)}})
	require.NoError(t, err)
	assert.Equal(t, int64(1+len("user")+len(s.Render())), n)
}
