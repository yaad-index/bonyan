package assemble_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

var counter = tokenize.ByteBound{PerMessage: 1}

func from(kind content.Kind, id, s string) content.Untrusted {
	return content.From(content.Provenance{Kind: kind, ID: id}, s)
}

func user(id, s string) model.Message {
	return model.Message{Role: model.RoleUser, Parts: []content.Text{from(content.KindUser, id, s)}}
}

func reply(s string) model.Message {
	return model.Message{Role: model.RoleAssistant, Parts: []content.Text{from(content.KindUser, "", s)}}
}

func calls(ids ...string) model.Message {
	m := model.Message{Role: model.RoleAssistant}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, model.ToolCall{ID: id, Name: "search", Arguments: json.RawMessage(`{}`)})
	}
	return m
}

func result(id, s string) model.Message {
	return model.Message{Role: model.RoleTool, ToolCallID: id, Parts: []content.Text{from(content.KindTool, id, s)}}
}

// roomy fits everything the tests build.
var roomy = assemble.Budgets{Instructions: 10_000, Memory: 10_000, Material: 10_000, History: 10_000, Tools: 10_000}

func sizeOf(t *testing.T, msgs ...model.Message) int64 {
	t.Helper()
	n, err := counter.Count(model.ChatRequest{Messages: msgs})
	require.NoError(t, err)
	return n
}

func TestTheSectionsComeInOrder(t *testing.T) {
	tools := []model.ToolDef{{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}}
	req, dropped, err := assemble.Build(assemble.Input{
		Instructions: content.Instruction("be brief"),
		Memory:       []content.Text{content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, "a fact")},
		Material:     []content.Text{from(content.KindFetched, "doc-1", "a page")},
		Earlier:      []model.Message{user("m0", "earlier"), reply("an answer")},
		Current:      []model.Message{user("m1", "now")},
		Tools:        tools,
	}, roomy, counter)
	require.NoError(t, err)
	assert.Empty(t, dropped)
	assert.Equal(t, tools, req.Tools)

	msgs := req.Messages
	require.Len(t, msgs, 5)
	assert.Equal(t, model.RoleSystem, msgs[0].Role)
	assert.Equal(t, []content.Text{content.Instruction("be brief")}, msgs[0].Parts)
	require.Len(t, msgs[1].Parts, 2, "memory, then material, in one message")
	assert.Equal(t, assemble.SectionMemory, msgs[1].Parts[0].(content.Section).Label())
	assert.Equal(t, assemble.SectionMaterial, msgs[1].Parts[1].(content.Section).Label())
	assert.Equal(t, "earlier", only(t, msgs[2]).Raw())
	assert.Equal(t, model.RoleAssistant, msgs[3].Role)
	assert.Equal(t, "now", only(t, msgs[4]).Raw())
}

func TestNoContextMessageWithoutMemoryOrMaterial(t *testing.T) {
	req, _, err := assemble.Build(assemble.Input{Instructions: content.Instruction("x"), Current: []model.Message{user("m1", "q")}}, roomy, counter)
	require.NoError(t, err)
	require.Len(t, req.Messages, 2)
	assert.Equal(t, model.RoleUser, req.Messages[1].Role)
}

// only returns the one item of the one section in m.
func only(t *testing.T, m model.Message) content.Untrusted {
	t.Helper()
	require.Len(t, m.Parts, 1)
	s, ok := m.Parts[0].(content.Section)
	require.True(t, ok, "untrusted text arrives inside a section, got %T", m.Parts[0])
	require.Len(t, s.Items(), 1)
	return s.Items()[0]
}

// Untrusted text appears only inside a section, never bare and never in the
// system message, whatever the input held.
func TestUntrustedTextOnlyInsideASection(t *testing.T) {
	req, _, err := assemble.Build(assemble.Input{
		Instructions: content.Instruction("x"),
		Memory:       []content.Text{content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, "a fact")},
		Material:     []content.Text{from(content.KindFetched, "d", "page")},
		Earlier:      []model.Message{user("m0", "earlier"), calls("c0"), result("c0", "found")},
		Current:      []model.Message{user("m1", "now")},
	}, roomy, counter)
	require.NoError(t, err)
	sections := 0
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			switch p.(type) {
			case content.Trusted:
				assert.Equal(t, model.RoleSystem, m.Role, "trusted text only in the system message")
			case content.Section:
				assert.NotEqual(t, model.RoleSystem, m.Role)
				sections++
			default:
				t.Errorf("a %T part in a %s message", p, m.Role)
			}
		}
	}
	assert.Equal(t, 5, sections)
}

func TestInputThePipelineCannotPlaceIsRefused(t *testing.T) {
	for name, in := range map[string]assemble.Input{
		"system message in history": {Earlier: []model.Message{{Role: model.RoleSystem, Parts: []content.Text{from(content.KindUser, "m0", "x")}}}},
		"a marked part in history":  {Earlier: []model.Message{{Role: model.RoleUser, Parts: []content.Text{content.NewMarked(content.NewSection("x"), "x")}}}},
		"a marked item in material": {Material: []content.Text{content.NewMarked(content.NewSection("x"), "x")}},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := assemble.Build(in, roomy, counter)
			require.ErrorIs(t, err, assemble.ErrInvalidInput)
		})
	}
}

// A section exactly at its budget fits.
func TestASectionAtItsBudgetFits(t *testing.T) {
	in := assemble.Input{Instructions: content.Instruction("be brief"), Current: []model.Message{user("m1", "q")}}
	b := roomy
	b.Instructions = sizeOf(t, model.Message{Role: model.RoleSystem, Parts: []content.Text{in.Instructions}})
	_, _, err := assemble.Build(in, b, counter)
	require.NoError(t, err)
	b.Instructions--
	_, _, err = assemble.Build(in, b, counter)
	require.ErrorIs(t, err, assemble.ErrOverBudget)
}

func TestBudgetsMustBePositive(t *testing.T) {
	require.NoError(t, assemble.DefaultBudgets().Validate())
	for _, b := range []assemble.Budgets{
		{},
		{Instructions: 1, Memory: 1, Material: 1, History: 1, Tools: 0},
		{Instructions: -1, Memory: 1, Material: 1, History: 1, Tools: 1},
		{Instructions: 1, Memory: 0, Material: 1, History: 1, Tools: 1},
		{Instructions: 1, Memory: 1, Material: 0, History: 1, Tools: 1},
		{Instructions: 1, Memory: 1, Material: 1, History: 0, Tools: 1},
	} {
		assert.ErrorIs(t, b.Validate(), assemble.ErrInvalidBudgets, "%+v", b)
		_, _, err := assemble.Build(assemble.Input{}, b, counter)
		assert.ErrorIs(t, err, assemble.ErrInvalidBudgets)
	}
}

// Instructions, tool definitions and this run's turns are never trimmed: what
// does not fit is an error.
func TestWhatCannotBeTrimmedFailsTyped(t *testing.T) {
	small := roomy
	small.Instructions = 5
	_, _, err := assemble.Build(assemble.Input{Instructions: content.Instruction(strings.Repeat("x", 50))}, small, counter)
	require.ErrorIs(t, err, assemble.ErrOverBudget)
	assert.Contains(t, err.Error(), assemble.SectionInstructions)

	small = roomy
	small.Tools = 5
	_, _, err = assemble.Build(assemble.Input{Tools: []model.ToolDef{{Name: "search", Description: strings.Repeat("x", 50)}}}, small, counter)
	require.ErrorIs(t, err, assemble.ErrOverBudget)
	assert.Contains(t, err.Error(), assemble.SectionTools)

	small = roomy
	small.History = 5
	_, _, err = assemble.Build(assemble.Input{Current: []model.Message{user("m1", strings.Repeat("x", 50))}}, small, counter)
	require.ErrorIs(t, err, assemble.ErrOverBudget)
	assert.Contains(t, err.Error(), "this run's turns")
}

func TestMemoryAndMaterialDropFromTheEnd(t *testing.T) {
	items := []content.Untrusted{from(content.KindFetched, "d0", "first"), from(content.KindFetched, "d1", "second"), from(content.KindFetched, "d2", "third")}
	texts := []content.Text{items[0], items[1], items[2]}
	fitTwo := sizeOf(t, model.Message{Role: model.RoleUser, Parts: []content.Text{content.NewSection(assemble.SectionMaterial, items[:2]...)}})
	b := roomy
	b.Material = fitTwo
	req, dropped, err := assemble.Build(assemble.Input{Material: texts}, b, counter)
	require.NoError(t, err)
	kept := req.Messages[1].Parts[0].(content.Section).Items()
	assert.Equal(t, items[:2], kept)
	require.Len(t, dropped, 1)
	want := sizeOf(t, model.Message{Role: model.RoleUser, Parts: []content.Text{content.NewSection(assemble.SectionMaterial, items[2])}})
	assert.Equal(t, assemble.Dropped{Section: assemble.SectionMaterial, Index: 2, Source: items[2].Provenance(), Tokens: want}, dropped[0])

	b = roomy
	b.Memory = 1
	mem := []content.Text{content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser, ID: "fact-7"}, "a fact")}
	req, dropped, err = assemble.Build(assemble.Input{Memory: mem}, b, counter)
	require.NoError(t, err)
	assert.Len(t, req.Messages, 1, "no context message when every item is dropped")
	require.Len(t, dropped, 1)
	assert.Equal(t, content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, dropped[0].Source, "a memory item's ID is never reported")

	// Nor when memory sits in another section, or the policy trusted it; a
	// trusted item's source is reported like an untrusted one's.
	b = roomy
	b.Material = 1
	memProv := content.Provenance{Kind: content.KindMemory, Origin: content.KindUser, ID: "fact-8"}
	fetched := content.Provenance{Kind: content.KindFetched, ID: "d9"}
	_, dropped, err = assemble.Build(assemble.Input{Material: []content.Text{
		content.From(memProv, "a fact"), content.TrustedFrom(memProv, "a trusted fact"), content.TrustedFrom(fetched, "a trusted page"),
	}}, b, counter)
	require.NoError(t, err)
	require.Len(t, dropped, 3)
	assert.Equal(t, content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, dropped[0].Source)
	assert.Equal(t, content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, dropped[1].Source)
	assert.Equal(t, fetched, dropped[2].Source)
}

// Earlier history is dropped oldest first, a whole exchange at a time, and
// never a turn of this run.
func TestHistoryDropsWholeExchangesOldestFirst(t *testing.T) {
	earlier := []model.Message{
		user("m0", "oldest question"),
		calls("c1", "c2"), result("c1", "one"), result("c2", "two"),
		reply("an answer"),
		user("m3", "latest question"),
	}
	current := []model.Message{user("m4", "now")}
	in := assemble.Input{Earlier: earlier, Current: current}
	sectioned, _, err := assemble.Build(in, roomy, counter)
	require.NoError(t, err)
	hist := sectioned.Messages[1:]
	// hist: m0, the call, its two results, the answer, m3, and this run's turn.
	// Room for the last two earlier messages and this run's turn, and one
	// short of also fitting the tool exchange before them.
	keep := sizeOf(t, hist[4]) + sizeOf(t, hist[5]) + sizeOf(t, hist[6])
	exchange := sizeOf(t, hist[1]) + sizeOf(t, hist[2]) + sizeOf(t, hist[3])
	b := roomy
	b.History = keep + exchange - 1

	req, dropped, err := assemble.Build(in, b, counter)
	require.NoError(t, err)
	got := req.Messages[1:]
	require.Len(t, got, 3)
	assert.Equal(t, "an answer", only(t, got[0]).Raw())
	assert.Equal(t, "latest question", only(t, got[1]).Raw())
	assert.Equal(t, "now", only(t, got[2]).Raw())

	var idx []int
	for _, d := range dropped {
		assert.Equal(t, assemble.SectionHistory, d.Section)
		idx = append(idx, d.Index)
	}
	assert.Equal(t, []int{0, 1, 2, 3}, idx, "the oldest message and the whole exchange after it")
	assert.Equal(t, content.Provenance{Kind: content.KindTool, ID: "c1"}, dropped[2].Source)
	assert.Equal(t, content.Provenance{}, dropped[1].Source, "an assistant message has no source")

	b.History = sizeOf(t, hist[6]) - 1
	_, _, err = assemble.Build(in, b, counter)
	require.ErrorIs(t, err, assemble.ErrOverBudget, "this run's turn is never dropped")
}

// No kept tool result is ever without the call it answers.
func TestTrimmingNeverOrphansAToolExchange(t *testing.T) {
	earlier := []model.Message{calls("c1", "c2"), result("c1", strings.Repeat("a", 40)), result("c2", "b"), user("m1", "q")}
	for budget := int64(1); budget < 400; budget++ {
		b := roomy
		b.History = budget
		req, _, err := assemble.Build(assemble.Input{Earlier: earlier}, b, counter)
		require.NoError(t, err)
		answered := map[string]bool{}
		for _, m := range req.Messages {
			for _, c := range m.ToolCalls {
				answered[c.ID] = false
			}
			if m.Role == model.RoleTool {
				_, called := answered[m.ToolCallID]
				require.True(t, called, "budget %d kept result %s without its call", budget, m.ToolCallID)
				answered[m.ToolCallID] = true
			}
		}
		for id, ok := range answered {
			require.True(t, ok, "budget %d kept call %s without its result", budget, id)
		}
	}
}

// The same input and budgets always give the same request and the same
// report of what was dropped.
func TestTrimmingIsDeterministic(t *testing.T) {
	in := assemble.Input{
		Instructions: content.Instruction("x"),
		Material:     []content.Text{from(content.KindFetched, "a", strings.Repeat("a", 30)), from(content.KindFetched, "b", strings.Repeat("b", 30))},
		Earlier:      []model.Message{user("m0", strings.Repeat("c", 30)), user("m1", "d")},
		Current:      []model.Message{user("m2", "now")},
	}
	full, _, err := assemble.Build(in, roomy, counter)
	require.NoError(t, err)
	// Room for the first material item and for the newer earlier message
	// beside this run's turn: one item of each is dropped.
	b := roomy
	b.Material = sizeOf(t, model.Message{Role: model.RoleUser, Parts: []content.Text{content.NewSection(assemble.SectionMaterial, in.Material[0].(content.Untrusted))}})
	b.History = sizeOf(t, full.Messages[3:]...)
	req1, d1, err := assemble.Build(in, b, counter)
	require.NoError(t, err)
	require.Len(t, d1, 2)
	for range 20 {
		req2, d2, err := assemble.Build(in, b, counter)
		require.NoError(t, err)
		assert.Equal(t, req1, req2)
		assert.Equal(t, d1, d2)
	}
}

// Text the trust policy declared trusted where it entered is placed as it is:
// in the conversation where it was, and after the section in the context
// message.
func TestTrustedTextIsPlacedOutsideTheSections(t *testing.T) {
	trusted := content.Instruction("a vetted page")
	req, _, err := assemble.Build(assemble.Input{
		Material: []content.Text{from(content.KindFetched, "d", "page"), trusted},
		Current:  []model.Message{{Role: model.RoleUser, Parts: []content.Text{content.Instruction("a vetted question")}}},
	}, roomy, counter)
	require.NoError(t, err)
	require.Len(t, req.Messages, 3)
	assert.Equal(t, assemble.SectionMaterial, req.Messages[1].Parts[0].(content.Section).Label())
	assert.Equal(t, trusted, req.Messages[1].Parts[1])
	assert.Equal(t, []content.Text{content.Instruction("a vetted question")}, req.Messages[2].Parts)
}
