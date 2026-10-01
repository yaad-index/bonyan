package mcpclient

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// How each keyword validation uses is kept. A keyword missing from this table
// is dropped, so a keyword a server invents never reaches the model.
type keyword int

const (
	value         keyword = iota + 1 // kept as it is: no subschema inside
	subschema                        // one schema
	schemaMap                        // an object of schemas, keyed by names or patterns
	schemaList                       // an array of schemas
	schemaOrList                     // one schema, or an array of them (draft-07 items)
	schemaOrNames                    // a schema, or an array of property names (draft-07 dependencies)
	identifier                       // $id, $anchor and $dynamicAnchor: kept when a reference resolves through it
	dialect                          // $schema: kept when it names a dialect validation knows
)

var keywords = map[string]keyword{
	// The dialect, which validation reads.
	"$schema": dialect,
	// References, what they resolve through, and the definitions they point to.
	"$ref": value, "$dynamicRef": value,
	"$id": identifier, "$anchor": identifier, "$dynamicAnchor": identifier,
	"$defs": schemaMap, "definitions": schemaMap,
	// Applicators.
	"allOf": schemaList, "anyOf": schemaList, "oneOf": schemaList, "not": subschema,
	"if": subschema, "then": subschema, "else": subschema,
	"dependentSchemas": schemaMap, "dependencies": schemaOrNames,
	"prefixItems": schemaList, "items": schemaOrList, "additionalItems": subschema,
	"contains": subschema, "unevaluatedItems": subschema,
	"properties": schemaMap, "patternProperties": schemaMap,
	"additionalProperties": subschema, "unevaluatedProperties": subschema, "propertyNames": subschema,
	// Validation.
	"type": value, "enum": value, "const": value,
	"multipleOf": value, "maximum": value, "exclusiveMaximum": value, "minimum": value, "exclusiveMinimum": value,
	"maxLength": value, "minLength": value, "pattern": value, "format": value,
	"maxItems": value, "minItems": value, "uniqueItems": value, "maxContains": value, "minContains": value,
	"maxProperties": value, "minProperties": value, "required": value, "dependentRequired": value,
}

// dialects are the values of $schema validation tells apart. It reads any
// other value as the newest of them, so dropping one changes nothing.
var dialects = map[string]bool{
	"http://json-schema.org/draft-07/schema#":      true,
	"https://json-schema.org/draft-07/schema#":     true,
	"https://json-schema.org/draft/2020-12/schema": true,
}

// root is the base a schema without an identifier of its own resolves
// references against. It never reaches the model.
var root = &url.URL{Scheme: "https", Host: "schema.invalid", Path: "/"}

// keepValidation returns schema with only the keywords validation uses, at
// every level. An identifier or anchor is kept only when a reference resolves
// through it; a reference that no longer resolves makes the schema fail to
// parse, rather than validate less.
func keepValidation(schema json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return nil, err
	}
	w := &walk{}
	if _, err := w.schema(v, root, true); err != nil {
		return nil, err
	}
	w.keep = true
	kept, err := w.schema(v, root, true)
	if err != nil {
		return nil, err
	}
	return json.Marshal(kept)
}

// walk goes over a schema twice, along the same keywords: the first time it
// records where each reference resolves to, the second it builds the copy.
type walk struct {
	keep bool
	refs []ref
}

type ref struct {
	target   string // the resolved reference, fragment included
	relative bool   // written as a fragment only, so it needs no identifier at the root
}

func (w *walk) schema(v any, base *url.URL, isRoot bool) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		// true and false are schemas too; anything else fails parsing later.
		return v, nil
	}
	if id, ok := m["$id"].(string); ok && id != "" && id[0] != '#' {
		// An identifier starts a new base; one written as a fragment is a
		// draft-07 anchor and does not.
		u, err := base.Parse(id)
		if err != nil {
			return nil, err
		}
		base = withoutFragment(u)
	}
	var out map[string]any
	if w.keep {
		out = make(map[string]any, len(m))
	}
	for k, val := range m {
		kind, known := keywords[k]
		if !known {
			continue
		}
		if !w.keep {
			if k == "$ref" || k == "$dynamicRef" {
				if s, ok := val.(string); ok {
					u, err := base.Parse(s)
					if err != nil {
						return nil, err
					}
					w.refs = append(w.refs, ref{target: u.String(), relative: strings.HasPrefix(s, "#")})
				}
			}
		}
		if kind == identifier {
			if w.keep && w.resolvesThrough(k, val, base, isRoot) {
				out[k] = val
			}
			continue
		}
		if kind == dialect {
			if d, ok := val.(string); ok && w.keep && dialects[d] {
				out[k] = val
			}
			continue
		}
		kept, err := w.keyword(kind, val, base)
		if err != nil {
			return nil, err
		}
		if w.keep {
			out[k] = kept
		}
	}
	if !w.keep {
		return nil, nil
	}
	return out, nil
}

// resolvesThrough reports whether a reference resolves through the identifier
// or anchor k in a schema whose base, after its own $id, is base.
func (w *walk) resolvesThrough(k string, val any, base *url.URL, isRoot bool) bool {
	s, ok := val.(string)
	if !ok || s == "" {
		return false
	}
	switch k {
	case "$id":
		if s[0] == '#' {
			// A draft-07 anchor written as an identifier.
			return w.targeted(base.String() + s)
		}
		for _, r := range w.refs {
			if r.relative && isRoot {
				continue
			}
			if stripFragment(r.target) == base.String() {
				return true
			}
		}
		return false
	default: // $anchor, $dynamicAnchor
		return w.targeted(base.String() + "#" + s)
	}
}

func (w *walk) targeted(target string) bool {
	for _, r := range w.refs {
		if r.target == target {
			return true
		}
	}
	return false
}

func (w *walk) keyword(kind keyword, val any, base *url.URL) (any, error) {
	switch kind {
	case subschema:
		return w.schema(val, base, false)
	case schemaMap:
		m, ok := val.(map[string]any)
		if !ok {
			return nil, errors.New("expected an object of schemas")
		}
		out := make(map[string]any, len(m))
		for name, s := range m {
			kept, err := w.schema(s, base, false)
			if err != nil {
				return nil, err
			}
			out[name] = kept
		}
		return out, nil
	case schemaList:
		return w.list(val, base)
	case schemaOrList:
		if _, ok := val.([]any); ok {
			return w.list(val, base)
		}
		return w.schema(val, base, false)
	case schemaOrNames:
		m, ok := val.(map[string]any)
		if !ok {
			return nil, errors.New("expected an object")
		}
		out := make(map[string]any, len(m))
		for name, d := range m {
			if names, ok := d.([]any); ok {
				out[name] = names
				continue
			}
			kept, err := w.schema(d, base, false)
			if err != nil {
				return nil, err
			}
			out[name] = kept
		}
		return out, nil
	}
	return val, nil
}

func (w *walk) list(val any, base *url.URL) (any, error) {
	l, ok := val.([]any)
	if !ok {
		return nil, errors.New("expected an array of schemas")
	}
	out := make([]any, len(l))
	for i, s := range l {
		kept, err := w.schema(s, base, false)
		if err != nil {
			return nil, err
		}
		out[i] = kept
	}
	return out, nil
}

func withoutFragment(u *url.URL) *url.URL {
	c := *u
	c.Fragment, c.RawFragment = "", ""
	return &c
}

func stripFragment(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}
