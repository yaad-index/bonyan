package tiktoken_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
	"github.com/yaad-index/bonyan/tokenize/tiktoken"
)

func user(s string) model.Message {
	return model.Message{Role: model.RoleUser, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser}, s)}}
}

func TestKnownCounts(t *testing.T) {
	cases := []struct {
		encoding string
		text     string
		want     int64
	}{
		{"cl100k_base", "hello world", 2},
		{"o200k_base", "hello world", 2},
		{"cl100k_base", "", 0},
	}
	for _, tc := range cases {
		c, err := tiktoken.New(tc.encoding, 1)
		require.NoError(t, err)
		n, err := c.Count(model.ChatRequest{Messages: []model.Message{{Role: "", Parts: []content.Text{content.Instruction(tc.text)}}}})
		require.NoError(t, err)
		assert.Equal(t, tc.want+1, n, "%s %q (+1 allowance)", tc.encoding, tc.text)
	}
}

func TestUnknownEncodingIsAnError(t *testing.T) {
	for _, name := range []string{"", "not-an-encoding", "a-model-name"} {
		_, err := tiktoken.New(name, 0)
		require.Error(t, err, "%q", name)
	}
}

// For a byte-level encoding the exact count never exceeds the byte bound, and
// it is lower for ordinary text. Both are what the budget relies on.
func TestExactCountIsWithinTheByteBound(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	req := model.ChatRequest{
		Messages: []model.Message{
			{Role: model.RoleSystem, Parts: []content.Text{content.Instruction("Answer briefly.")}},
			user("سلام دنیا، 日本語のテキスト, emoji 🙂🙂🙂 and Grüße"),
			user(strings.Repeat("the quick brown fox ", 50)),
			model.ToolResult("c1", `{"items":[1,2,3]}`),
		},
		Tools:  []model.ToolDef{{Name: "search", Description: "find things", Parameters: params}},
		Schema: json.RawMessage(`{"type":"object"}`),
	}
	for _, enc := range []string{"cl100k_base", "o200k_base", "r50k_base", "p50k_base"} {
		exact, err := tiktoken.New(enc, 0)
		require.NoError(t, err)
		n, err := exact.Count(req)
		require.NoError(t, err)
		bound, err := tokenize.ByteBound{}.Count(req)
		require.NoError(t, err)
		assert.LessOrEqual(t, n, bound, enc)
		assert.Less(t, n, bound/2, "%s: ordinary text is far below one token per byte", enc)
	}
}

func TestEveryPartIsCounted(t *testing.T) {
	c, err := tiktoken.New("cl100k_base", 3)
	require.NoError(t, err)
	base := model.ChatRequest{Messages: []model.Message{user("hello")}}
	n0, err := c.Count(base)
	require.NoError(t, err)

	withTool := base
	withTool.Tools = []model.ToolDef{{Name: "search"}}
	n1, err := c.Count(withTool)
	require.NoError(t, err)
	assert.Greater(t, n1, n0+3, "a tool adds its allowance and its name")

	withSchema := base
	withSchema.Schema = json.RawMessage(`{"type":"object"}`)
	n2, err := c.Count(withSchema)
	require.NoError(t, err)
	assert.Greater(t, n2, n0)

	twoParts := model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Parts: []content.Text{
		content.Instruction("hello"), content.From(content.Provenance{Kind: content.KindFetched}, " again"),
	}}}}
	n3, err := c.Count(twoParts)
	require.NoError(t, err)
	assert.Greater(t, n3, n0, "untrusted parts are counted too")
}

func TestDefaultAllowance(t *testing.T) {
	c, err := tiktoken.New("cl100k_base", 0)
	require.NoError(t, err)
	n, err := c.Count(model.ChatRequest{Messages: []model.Message{{}}})
	require.NoError(t, err)
	assert.Equal(t, int64(tokenize.DefaultPerMessage), n)
}

func TestSafeForConcurrentUse(t *testing.T) {
	c, err := tiktoken.New("o200k_base", 0)
	require.NoError(t, err)
	req := model.ChatRequest{Messages: []model.Message{user(strings.Repeat("concurrent counting ", 20))}}
	want, err := c.Count(req)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := c.Count(req)
			assert.NoError(t, err)
			assert.Equal(t, want, n)
		}()
	}
	wg.Wait()
}
