// Package assemble builds the context for each model call through an ordered
// pipeline (ADR 0001 §3): the instructions, recalled memory, material, the
// conversation history and the tool definitions, each held to a token budget.
//
// Untrusted text enters only inside a content.Section. Trimming drops whole
// items, is deterministic, and reports every dropped item, so it is always
// possible to say what was left out of a call and why.
package assemble

import (
	"errors"
	"fmt"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

// The section names, as Dropped and recordings report them.
const (
	SectionInstructions = "instructions"
	SectionMemory       = "memory"
	SectionMaterial     = "material"
	SectionHistory      = "history"
	SectionTools        = "tools"
)

// Budgets are the token budgets per section, counted by the run's
// tokenize.Counter.
type Budgets struct {
	Instructions int64
	Memory       int64
	Material     int64
	History      int64
	Tools        int64
}

// DefaultBudgets are the budgets used when none are configured. They are
// starting values, not sized against any model's context window; a program
// sets its own.
func DefaultBudgets() Budgets {
	return Budgets{Instructions: 8_000, Memory: 8_000, Material: 32_000, History: 64_000, Tools: 16_000}
}

// ErrInvalidBudgets reports budgets that would leave a section unbounded.
var ErrInvalidBudgets = errors.New("assemble: invalid budgets")

// Validate refuses a zero or negative budget, which is never "unlimited".
func (b Budgets) Validate() error {
	var errs []error
	for _, s := range []struct {
		name string
		v    int64
	}{
		{SectionInstructions, b.Instructions},
		{SectionMemory, b.Memory},
		{SectionMaterial, b.Material},
		{SectionHistory, b.History},
		{SectionTools, b.Tools},
	} {
		if s.v <= 0 {
			errs = append(errs, fmt.Errorf("%s budget must be positive, got %d", s.name, s.v))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidBudgets, errors.Join(errs...))
}

// ErrOverBudget reports a section that cannot be trimmed and does not fit: the
// instructions, the tool definitions, or this run's own turns.
var ErrOverBudget = errors.New("assemble: over budget")

// ErrInvalidInput reports input the pipeline cannot place: a system message
// in the conversation, or an item that is neither trusted nor untrusted text.
var ErrInvalidInput = errors.New("assemble: invalid input")

// Input is what one call's context is built from.
type Input struct {
	// Instructions are the system instructions. They are never trimmed.
	Instructions content.Trusted
	// Memory is recalled memory, most relevant first. Items are dropped from
	// the end. Untrusted items go inside the section; an item the trust policy
	// declared trusted follows it as plain text.
	Memory []content.Text
	// Material is retrieved or fetched material, most relevant first, placed
	// like Memory. Items are dropped from the end.
	Material []content.Text
	// Earlier is the conversation before this run. It is trimmed oldest
	// first, a whole exchange at a time: an assistant message that called
	// tools goes together with the results answering it.
	Earlier []model.Message
	// Current are this run's turns, the user message first. They are never
	// trimmed: losing them would make the model ask again for what it has.
	Current []model.Message
	// Tools are the tool definitions. They are never trimmed.
	Tools []model.ToolDef
}

// Dropped is one item left out of a call.
type Dropped struct {
	// Section is SectionMemory, SectionMaterial or SectionHistory.
	Section string
	// Index is the item's position in its Input field.
	Index int
	// Source is where the item came from; for a history message, its first
	// untrusted item's. It is zero for an assistant message and for trusted
	// text, and a memory item's ID is never reported.
	Source content.Provenance
	// Tokens is what the item counted.
	Tokens int64
}

// Build assembles one call's request: the instructions as the system message,
// memory and material as sections in one message after it, then the kept
// history and this run's turns, with every untrusted part of a history
// message put inside a section, and the tool definitions. It returns what it
// dropped, in section order and item order.
func Build(in Input, b Budgets, c tokenize.Counter) (model.ChatRequest, []Dropped, error) {
	if err := b.Validate(); err != nil {
		return model.ChatRequest{}, nil, err
	}
	earlier, err := sectioned(in.Earlier)
	if err != nil {
		return model.ChatRequest{}, nil, err
	}
	current, err := sectioned(in.Current)
	if err != nil {
		return model.ChatRequest{}, nil, err
	}

	system := model.Message{Role: model.RoleSystem, Parts: []content.Text{in.Instructions}}
	if err := fits(c, SectionInstructions, b.Instructions, []model.Message{system}, nil); err != nil {
		return model.ChatRequest{}, nil, err
	}
	if err := fits(c, SectionTools, b.Tools, nil, in.Tools); err != nil {
		return model.ChatRequest{}, nil, err
	}

	var dropped []Dropped
	var context []content.Text
	for _, s := range []struct {
		name   string
		items  []content.Text
		budget int64
	}{
		{SectionMemory, in.Memory, b.Memory},
		{SectionMaterial, in.Material, b.Material},
	} {
		kept, d, err := trimItems(c, s.name, s.items, s.budget)
		if err != nil {
			return model.ChatRequest{}, nil, err
		}
		dropped = append(dropped, d...)
		context = append(context, place(s.name, kept)...)
	}

	history, d, err := trimHistory(c, earlier, current, b.History)
	if err != nil {
		return model.ChatRequest{}, nil, err
	}
	dropped = append(dropped, d...)

	msgs := []model.Message{system}
	if len(context) > 0 {
		msgs = append(msgs, model.Message{Role: model.RoleUser, Parts: context})
	}
	msgs = append(msgs, history...)
	return model.ChatRequest{Messages: msgs, Tools: in.Tools}, dropped, nil
}

// sectioned returns msgs with every bare untrusted part put inside a section
// of its own, and refuses a system message. Trusted text there is what the
// trust policy declared trusted where it entered; it stays as it is.
func sectioned(msgs []model.Message) ([]model.Message, error) {
	out := make([]model.Message, len(msgs))
	for i, m := range msgs {
		if m.Role == model.RoleSystem {
			return nil, fmt.Errorf("%w: a system message in the conversation", ErrInvalidInput)
		}
		parts := make([]content.Text, len(m.Parts))
		for j, p := range m.Parts {
			switch v := p.(type) {
			case content.Untrusted:
				parts[j] = content.NewSection(SectionHistory, v)
			case content.Section, content.Trusted:
				parts[j] = v
			default:
				return nil, fmt.Errorf("%w: a %T part in the conversation", ErrInvalidInput, p)
			}
		}
		m.Parts = parts
		out[i] = m
	}
	return out, nil
}

func count(c tokenize.Counter, msgs []model.Message, tools []model.ToolDef) (int64, error) {
	if len(msgs) == 0 && len(tools) == 0 {
		return 0, nil
	}
	return c.Count(model.ChatRequest{Messages: msgs, Tools: tools})
}

func fits(c tokenize.Counter, section string, budget int64, msgs []model.Message, tools []model.ToolDef) error {
	n, err := count(c, msgs, tools)
	if err != nil {
		return err
	}
	if n > budget {
		return fmt.Errorf("%w: %s counts %d of a %d budget", ErrOverBudget, section, n, budget)
	}
	return nil
}

// place returns items as the parts of the context message: the untrusted ones
// in one section labelled section, then the trusted ones.
func place(section string, items []content.Text) []content.Text {
	var untrusted []content.Untrusted
	var trusted []content.Text
	for _, it := range items {
		if u, ok := it.(content.Untrusted); ok {
			untrusted = append(untrusted, u)
		} else {
			trusted = append(trusted, it)
		}
	}
	var out []content.Text
	if len(untrusted) > 0 {
		out = append(out, content.NewSection(section, untrusted...))
	}
	return append(out, trusted...)
}

// trimItems keeps the longest prefix of items whose parts fit budget and
// reports the rest as dropped.
func trimItems(c tokenize.Counter, section string, items []content.Text, budget int64) ([]content.Text, []Dropped, error) {
	for _, it := range items {
		switch it.(type) {
		case content.Untrusted, content.Trusted:
		default:
			return nil, nil, fmt.Errorf("%w: a %T item in %s", ErrInvalidInput, it, section)
		}
	}
	n := len(items)
	for ; n > 0; n-- {
		used, err := count(c, []model.Message{{Role: model.RoleUser, Parts: place(section, items[:n])}}, nil)
		if err != nil {
			return nil, nil, err
		}
		if used <= budget {
			break
		}
	}
	var dropped []Dropped
	for i := n; i < len(items); i++ {
		t, err := count(c, []model.Message{{Role: model.RoleUser, Parts: place(section, items[i:i+1])}}, nil)
		if err != nil {
			return nil, nil, err
		}
		var src content.Provenance
		switch v := items[i].(type) {
		case content.Untrusted:
			src = v.Provenance()
		case content.Trusted:
			src = v.Provenance()
		}
		// A memory item's ID is never reported, whichever section held it.
		if section == SectionMemory || src.Kind == content.KindMemory {
			src.ID = ""
		}
		dropped = append(dropped, Dropped{Section: section, Index: i, Source: src, Tokens: t})
	}
	return items[:n], dropped, nil
}

// trimHistory keeps every current turn and the newest earlier exchanges that
// fit beside them.
func trimHistory(c tokenize.Counter, earlier, current []model.Message, budget int64) ([]model.Message, []Dropped, error) {
	used, err := count(c, current, nil)
	if err != nil {
		return nil, nil, err
	}
	if used > budget {
		return nil, nil, fmt.Errorf("%w: this run's turns count %d of a %d history budget", ErrOverBudget, used, budget)
	}
	units := exchanges(earlier)
	sizes := make([][]int64, len(units))
	for u, idx := range units {
		for _, i := range idx {
			t, err := count(c, earlier[i:i+1], nil)
			if err != nil {
				return nil, nil, err
			}
			sizes[u] = append(sizes[u], t)
		}
	}
	first := len(units)
	for u := len(units) - 1; u >= 0; u-- {
		var t int64
		for _, s := range sizes[u] {
			t += s
		}
		if used+t > budget {
			break
		}
		used += t
		first = u
	}
	var kept []model.Message
	var dropped []Dropped
	for u, idx := range units {
		for k, i := range idx {
			if u < first {
				dropped = append(dropped, Dropped{Section: SectionHistory, Index: i, Source: source(earlier[i]), Tokens: sizes[u][k]})
				continue
			}
			kept = append(kept, earlier[i])
		}
	}
	return append(kept, current...), dropped, nil
}

// exchanges groups msgs into the units history is dropped by: an assistant
// message that called tools with the tool messages that follow it, and every
// other message on its own.
func exchanges(msgs []model.Message) [][]int {
	var units [][]int
	for i := 0; i < len(msgs); i++ {
		unit := []int{i}
		if msgs[i].Role == model.RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			for i+1 < len(msgs) && msgs[i+1].Role == model.RoleTool {
				i++
				unit = append(unit, i)
			}
		}
		units = append(units, unit)
	}
	return units
}

func source(m model.Message) content.Provenance {
	for _, p := range m.Parts {
		if s, ok := p.(content.Section); ok {
			if items := s.Items(); len(items) > 0 {
				return items[0].Provenance()
			}
		}
	}
	return content.Provenance{}
}
