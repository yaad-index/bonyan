package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool"
)

type lookup struct {
	City string `json:"city"`
	Days int    `json:"days"`
}

type forecast struct {
	Summary string `json:"summary"`
}

type source map[string]string

func (s source) Lookup(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

func newRegistry(t *testing.T) (*tool.Registry, *[]lookup) {
	t.Helper()
	r := tool.NewRegistry(secret.NewResolver(source{"weather_key": "k-1", "other_key": "k-2"}))
	var got []lookup
	require.NoError(t, tool.Register(r, "weather", tool.Spec{Description: "the forecast", Secrets: []string{"weather_key"}, Network: true},
		func(ctx context.Context, in lookup, secrets *secret.Scoped) (forecast, error) {
			got = append(got, in)
			key, err := secrets.Resolve(ctx, "weather_key")
			if err != nil {
				return forecast{}, err
			}
			if _, err := secrets.Resolve(ctx, "other_key"); err == nil {
				return forecast{}, errors.New("resolved a secret it was not granted")
			}
			return forecast{Summary: in.City + " with " + key.Reveal()}, nil
		}))
	return r, &got
}

func call(args string) model.ToolCall {
	return model.ToolCall{ID: "c1", Name: "weather", Arguments: json.RawMessage(args)}
}

func TestAToolIsDescribedByItsInputType(t *testing.T) {
	r, _ := newRegistry(t)
	defs := r.Definitions()
	require.Len(t, defs, 1)
	assert.Equal(t, "weather", defs[0].Name)
	assert.Equal(t, "the forecast", defs[0].Description)
	var s map[string]any
	require.NoError(t, json.Unmarshal(defs[0].Parameters, &s))
	assert.Equal(t, "object", s["type"])
	assert.ElementsMatch(t, []any{"city", "days"}, s["required"])
	spec, ok := r.Spec("weather")
	require.True(t, ok)
	assert.True(t, spec.Network)
	assert.False(t, spec.Filesystem)
}

func TestACallRunsWithOnlyItsGrantedSecrets(t *testing.T) {
	r, got := newRegistry(t)
	out, err := r.Call(context.Background(), call(`{"city":"Lyon","days":2}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"summary":"Lyon with k-1"}`, out)
	assert.Equal(t, []lookup{{City: "Lyon", Days: 2}}, *got)
}

func TestInvalidArgumentsNeverReachTheTool(t *testing.T) {
	r, got := newRegistry(t)
	for name, args := range map[string]string{
		"missing field": `{"city":"Lyon"}`,
		"wrong type":    `{"city":"Lyon","days":"two"}`,
		"extra field":   `{"city":"Lyon","days":2,"x":1}`,
		"not JSON":      `{`,
		"absent":        ``,
	} {
		_, err := r.Call(context.Background(), call(args))
		require.ErrorIs(t, err, tool.ErrInvalidArguments, name)
		var ve *tool.ValidationError
		assert.ErrorAs(t, err, &ve, name)
	}
	assert.Empty(t, *got)
}

func TestUnknownAndDuplicateTools(t *testing.T) {
	r, _ := newRegistry(t)
	_, err := r.Call(context.Background(), model.ToolCall{Name: "missing"})
	require.ErrorIs(t, err, tool.ErrUnknown)
	_, ok := r.Spec("missing")
	assert.False(t, ok)
	err = tool.Register(r, "weather", tool.Spec{}, func(context.Context, lookup, *secret.Scoped) (forecast, error) { return forecast{}, nil })
	require.ErrorIs(t, err, tool.ErrDuplicate)
	require.Error(t, tool.Register(r, "", tool.Spec{}, func(context.Context, lookup, *secret.Scoped) (forecast, error) { return forecast{}, nil }))
	require.Error(t, tool.Register[lookup, forecast](r, "nil", tool.Spec{}, nil))
	require.Error(t, tool.Register(r, "bad", tool.Spec{}, func(context.Context, map[int]string, *secret.Scoped) (forecast, error) { return forecast{}, nil }))
}

func TestToolsAreDescribedInRegistrationOrder(t *testing.T) {
	r := tool.NewRegistry(nil)
	for _, n := range []string{"b", "a", "c"} {
		require.NoError(t, tool.Register(r, n, tool.Spec{}, func(context.Context, lookup, *secret.Scoped) (forecast, error) { return forecast{}, nil }))
	}
	var names []string
	for _, d := range r.Definitions() {
		names = append(names, d.Name)
	}
	assert.Equal(t, []string{"b", "a", "c"}, names)
}

// A validation error names the schema path and the rule, and never a value
// from the data: a marker placed in every kind of failure never comes back.
func TestAValidationErrorNeverQuotesTheData(t *testing.T) {
	type item struct {
		Name  string  `json:"name"`
		Price float64 `json:"price"`
	}
	type order struct {
		Items []item `json:"items"`
		Code  string `json:"code"`
	}
	s, err := tool.SchemaFor[order]()
	require.NoError(t, err)
	const marker = "MARKER-7f3a"
	for name, c := range map[string]struct{ data, want string }{
		"missing property": {`{"items":[{"name":"` + marker + `"}],"code":"x"}`, "/properties/items/items: required"},
		"wrong type deep":  {`{"items":[{"name":"a","price":"` + marker + `"}],"code":"x"}`, "/properties/items/items/properties/price: type"},
		"wrong type":       {`{"items":[],"code":"` + marker + `"}`, ""},
		"number for text":  {`{"items":[],"code":7}`, "/properties/code: type"},
		"string for list":  {`{"items":"` + marker + `","code":"x"}`, "/properties/items: type"},
		"extra property":   {`{"items":[],"code":"x","` + marker + `":1}`, "/: additionalProperties"},
		"not JSON":         {`not json ` + marker, "not valid JSON"},
	} {
		err := s.Validate([]byte(c.data))
		if c.want == "" {
			assert.NoError(t, err, name)
			continue
		}
		require.Error(t, err, name)
		assert.Equal(t, c.want, err.Error(), name)
		assert.NotContains(t, err.Error(), marker, name)
	}
}

func TestAMessageThatDoesNotParseSaysOnlyThatItFailed(t *testing.T) {
	s, err := tool.ParseSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`))
	require.NoError(t, err)
	require.NoError(t, s.Validate([]byte(`{"a":"x"}`)))
	assert.EqualError(t, s.Validate([]byte(`{}`)), "/: required")
	_, err = tool.ParseSchema(json.RawMessage(`{`))
	require.Error(t, err)
	assert.Equal(t, "does not match the schema", (&tool.ValidationError{}).Error())
}

// The generator's known gaps (implementation plan §3), each pinned so that an
// upgrade that changes one is noticed.
func TestKnownGeneratorGaps(t *testing.T) {
	t.Run("format is not enforced", func(t *testing.T) {
		raw, err := json.Marshal(&jsonschema.Schema{Type: "string", Format: "email"})
		require.NoError(t, err)
		s, err := tool.ParseSchema(raw)
		require.NoError(t, err)
		assert.NoError(t, s.Validate([]byte(`"not an address"`)))
	})
	t.Run("omitempty fields are never required", func(t *testing.T) {
		type in struct {
			A string `json:"a,omitempty"`
		}
		s, err := tool.SchemaFor[in]()
		require.NoError(t, err)
		assert.NoError(t, s.Validate([]byte(`{}`)))
	})
	t.Run("a slice also accepts null", func(t *testing.T) {
		type in struct {
			L []string `json:"l"`
		}
		s, err := tool.SchemaFor[in]()
		require.NoError(t, err)
		assert.NoError(t, s.Validate([]byte(`{"l":null}`)))
	})
}

func TestAToolWithNoRequiredInputTakesEmptyArguments(t *testing.T) {
	type none struct {
		Hint string `json:"hint,omitempty"`
	}
	r := tool.NewRegistry(nil)
	require.NoError(t, tool.Register(r, "ping", tool.Spec{}, func(context.Context, none, *secret.Scoped) (string, error) { return "pong", nil }))
	out, err := r.Call(context.Background(), model.ToolCall{Name: "ping"})
	require.NoError(t, err)
	assert.Equal(t, `"pong"`, out)
}

func TestAToolsErrorIsReturned(t *testing.T) {
	boom := errors.New("upstream down")
	r := tool.NewRegistry(nil)
	require.NoError(t, tool.Register(r, "w", tool.Spec{}, func(context.Context, lookup, *secret.Scoped) (forecast, error) { return forecast{}, boom }))
	_, err := r.Call(context.Background(), model.ToolCall{Name: "w", Arguments: json.RawMessage(`{"city":"a","days":1}`)})
	require.ErrorIs(t, err, boom)
}

func TestASchemasJSONIsACopy(t *testing.T) {
	s, err := tool.SchemaFor[lookup]()
	require.NoError(t, err)
	j := s.JSON()
	j[0] = 'X'
	assert.Equal(t, byte('{'), s.JSON()[0])
	assert.False(t, s.IsZero())
	assert.True(t, tool.Schema{}.IsZero())
	assert.Error(t, tool.Schema{}.Validate([]byte(`{}`)))
}

// Against the real validator, for every keyword it checks: whatever the failing
// value holds, the error carries none of it. A dependency update that changes a
// message to quote the data fails here.
func TestNoValidatorKeywordQuotesTheData(t *testing.T) {
	const marker = "MARKER-9c1e"
	str := `"` + marker + `: x"`
	for keyword, c := range map[string]struct{ schema, data string }{
		"type":                 {`{"type":"integer"}`, str},
		"enum":                 {`{"enum":["a","b"]}`, str},
		"const":                {`{"const":"a"}`, str},
		"pattern":              {`{"type":"string","pattern":"^a$"}`, str},
		"minLength":            {`{"type":"string","minLength":100}`, str},
		"maxLength":            {`{"type":"string","maxLength":1}`, str},
		"minimum":              {`{"type":"number","minimum":10}`, `1`},
		"maximum":              {`{"type":"number","maximum":0}`, `1`},
		"exclusiveMinimum":     {`{"type":"number","exclusiveMinimum":1}`, `1`},
		"exclusiveMaximum":     {`{"type":"number","exclusiveMaximum":1}`, `1`},
		"multipleOf":           {`{"type":"number","multipleOf":3}`, `1`},
		"minItems":             {`{"type":"array","minItems":5}`, `[` + str + `]`},
		"maxItems":             {`{"type":"array","maxItems":0}`, `[` + str + `]`},
		"uniqueItems":          {`{"type":"array","uniqueItems":true}`, `[` + str + `,` + str + `]`},
		"contains":             {`{"type":"array","contains":{"const":"a"}}`, `[` + str + `]`},
		"items":                {`{"type":"array","items":{"type":"integer"}}`, `[` + str + `]`},
		"required":             {`{"type":"object","required":["a"]}`, `{"` + marker + `":1}`},
		"additionalProperties": {`{"type":"object","additionalProperties":false}`, `{"` + marker + `":1}`},
		"propertyNames":        {`{"type":"object","propertyNames":{"pattern":"^a$"}}`, `{"` + marker + `":1}`},
		"minProperties":        {`{"type":"object","minProperties":3}`, `{"` + marker + `":1}`},
		"maxProperties":        {`{"type":"object","maxProperties":0}`, `{"` + marker + `":1}`},
		"dependentRequired":    {`{"type":"object","dependentRequired":{"a":["b"]}}`, `{"a":` + str + `}`},
		"anyOf":                {`{"anyOf":[{"type":"integer"},{"type":"boolean"}]}`, str},
		"oneOf":                {`{"oneOf":[{"type":"integer"},{"type":"boolean"}]}`, str},
		"allOf":                {`{"allOf":[{"type":"integer"}]}`, str},
		"not":                  {`{"not":{"type":"string"}}`, str},
		"if/then":              {`{"if":{"type":"string"},"then":{"const":"a"}}`, str},
	} {
		s, err := tool.ParseSchema(json.RawMessage(c.schema))
		require.NoError(t, err, keyword)
		err = s.Validate([]byte(c.data))
		require.Error(t, err, "%s should fail", keyword)
		assert.NotContains(t, err.Error(), marker, keyword)
		assert.NotContains(t, err.Error(), "x\"", keyword)
	}
}

// A tool whose schema is given rather than derived is validated against it and
// gets only the secrets it declared, as a typed one does.
func TestAToolWithAGivenSchema(t *testing.T) {
	r := tool.NewRegistry(secret.NewResolver(source{"weather_key": "k-1", "other_key": "k-2"}))
	schema, err := tool.ParseSchema(json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`))
	require.NoError(t, err)
	var got []string
	require.NoError(t, tool.RegisterSchema(r, "weather", tool.Spec{Description: "the forecast", Secrets: []string{"weather_key"}}, schema,
		func(ctx context.Context, args json.RawMessage, secrets *secret.Scoped) (string, error) {
			got = append(got, string(args))
			key, err := secrets.Resolve(ctx, "weather_key")
			if err != nil {
				return "", err
			}
			if _, err := secrets.Resolve(ctx, "other_key"); err == nil {
				return "", errors.New("resolved a secret it was not granted")
			}
			return "sunny with " + key.Reveal(), nil
		}))

	defs := r.Definitions()
	require.Len(t, defs, 1)
	assert.JSONEq(t, string(schema.JSON()), string(defs[0].Parameters))
	out, err := r.Call(context.Background(), call(`{"city":"Berlin"}`))
	require.NoError(t, err)
	assert.Equal(t, "sunny with k-1", out)
	_, err = r.Call(context.Background(), call(`{"city":7}`))
	require.ErrorIs(t, err, tool.ErrInvalidArguments)
	assert.Equal(t, []string{`{"city":"Berlin"}`}, got, "the invalid call never ran")
	assert.Equal(t, content.KindTool, r.Source("weather"))

	require.Error(t, tool.RegisterSchema(r, "empty", tool.Spec{}, tool.Schema{},
		func(context.Context, json.RawMessage, *secret.Scoped) (string, error) { return "", nil }))
}

// Every tool registered in the program reports its results as tool output; only
// the tool-server adapter can register one whose results are remote.
func TestATypedToolsResultsAreToolOutput(t *testing.T) {
	r, _ := newRegistry(t)
	assert.Equal(t, content.KindTool, r.Source("weather"))
	assert.Equal(t, content.KindTool, r.Source("missing"))
}
