package mcpclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
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
	definitions                      // $defs and definitions: an object of schemas, each kept when a reference reaches it
)

var keywords = map[string]keyword{
	// The dialect, which validation reads.
	"$schema": dialect,
	// References, what they resolve through, and the definitions they point to.
	"$ref": value, "$dynamicRef": value,
	"$id": identifier, "$anchor": identifier, "$dynamicAnchor": identifier,
	// The keys of $defs and definitions are definition names: one is kept
	// only when a reference reaches it ("the names of the definitions a
	// reference points to").
	"$defs": definitions, "definitions": definitions,
	// Applicators.
	"allOf": schemaList, "anyOf": schemaList, "oneOf": schemaList, "not": subschema,
	"if": subschema, "then": subschema, "else": subschema,
	// The keys of dependentSchemas and dependencies, and the names in a
	// dependencies array, are property names, and must be properties the
	// kept schema declares ("property names").
	"dependentSchemas": schemaMap, "dependencies": schemaOrNames,
	"prefixItems": schemaList, "items": schemaOrList, "additionalItems": subschema,
	"contains": subschema, "unevaluatedItems": subschema,
	// The keys of properties are property names; those of patternProperties
	// are patterns ("property names", "patterns").
	"properties": schemaMap, "patternProperties": schemaMap,
	"additionalProperties": subschema, "unevaluatedProperties": subschema, "propertyNames": subschema,
	// Validation.
	"type": value, "enum": value, "const": value,
	"multipleOf": value, "maximum": value, "exclusiveMaximum": value, "minimum": value, "exclusiveMinimum": value,
	"maxLength": value, "minLength": value, "pattern": value, "format": value,
	"maxItems": value, "minItems": value, "uniqueItems": value, "maxContains": value, "minContains": value,
	// The names in required, and the keys and names of dependentRequired, are
	// property names, checked like dependentSchemas'.
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
	// Which definitions a reference reaches depends on which are kept, since
	// a kept definition's own references count, so the first pass runs until
	// no more are reached. It records references and declared properties only
	// in what it keeps.
	w := &walk{keptDefs: map[string]bool{}}
	for {
		w.refs, w.properties, w.defs = nil, map[string]bool{}, map[string]def{}
		if _, err := w.schema(v, root, true); err != nil {
			return nil, err
		}
		reached := false
		for key, d := range w.defs {
			if !w.keptDefs[key] && w.reaches(key, d) {
				w.keptDefs[key], reached = true, true
			}
		}
		if !reached {
			break
		}
	}
	w.keep = true
	kept, err := w.schema(v, root, true)
	if err != nil {
		return nil, err
	}
	return json.Marshal(kept)
}

// walk goes over a schema along the same keywords each time: first, until it
// settles, to record where each reference resolves to and which definitions
// are reached; then once more to build the copy.
type walk struct {
	keep bool
	refs []ref
	// properties holds every property name the kept schema declares, at any
	// level.
	properties map[string]bool
	// defs are the definitions seen on the last pass, by the pointer that
	// reaches them; keptDefs are those a reference reaches.
	defs     map[string]def
	keptDefs map[string]bool
}

// def is what can reach a definition besides its pointer: its own identifier
// and anchors, as the targets a reference would resolve to.
type def struct {
	id      string
	anchors []string
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
			if k == "properties" {
				if props, ok := val.(map[string]any); ok {
					for name := range props {
						w.properties[name] = true
					}
				}
			}
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
		if w.keep {
			if err := w.namesProperties(k, val); err != nil {
				return nil, err
			}
		}
		if kind == dialect {
			if d, ok := val.(string); ok && w.keep && dialects[d] {
				out[k] = val
			}
			continue
		}
		if kind == definitions {
			kept, err := w.definitions(k, val, base)
			if err != nil {
				return nil, err
			}
			if w.keep && len(kept) > 0 {
				out[k] = kept
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

// namesProperties checks that every name required, or depended on, is a
// property the schema declares somewhere. Such a name reaches the model, and
// one naming no property would be text the server wrote that is not a property
// name.
func (w *walk) namesProperties(k string, val any) error {
	var names []any
	switch k {
	case "required":
		l, _ := val.([]any)
		names = l
	case "dependentRequired", "dependencies", "dependentSchemas":
		m, _ := val.(map[string]any)
		for key, d := range m {
			names = append(names, key)
			if l, ok := d.([]any); ok {
				names = append(names, l...)
			}
		}
	default:
		return nil
	}
	for _, n := range names {
		s, ok := n.(string)
		if !ok || !w.properties[s] {
			return fmt.Errorf("%s names something that is not a declared property", k)
		}
	}
	return nil
}

// definitions walks the definitions under k, keeping only those a reference
// reaches. Every definition is recorded, kept or not, so the next pass can
// see whether a newly kept one's references reach it.
func (w *walk) definitions(k string, val any, base *url.URL) (map[string]any, error) {
	m, ok := val.(map[string]any)
	if !ok {
		return nil, errors.New("expected an object of schemas")
	}
	escape := strings.NewReplacer("~", "~0", "/", "~1")
	out := map[string]any{}
	for name, s := range m {
		at := *base
		at.Fragment, at.RawFragment = "/"+k+"/"+escape.Replace(name), ""
		key := at.String()
		if !w.keep {
			w.defs[key] = defOf(s, base)
		}
		if !w.keptDefs[key] {
			continue
		}
		kept, err := w.schema(s, base, false)
		if err != nil {
			return nil, err
		}
		out[name] = kept
	}
	return out, nil
}

// defOf is what can reach the definition s besides its pointer.
func defOf(s any, base *url.URL) def {
	m, ok := s.(map[string]any)
	if !ok {
		return def{}
	}
	var d def
	if id, ok := m["$id"].(string); ok && id != "" {
		if id[0] == '#' {
			d.anchors = append(d.anchors, base.String()+id)
		} else if u, err := base.Parse(id); err == nil {
			base = withoutFragment(u)
			d.id = base.String()
		}
	}
	for _, k := range []string{"$anchor", "$dynamicAnchor"} {
		if a, ok := m[k].(string); ok && a != "" {
			d.anchors = append(d.anchors, base.String()+"#"+a)
		}
	}
	return d
}

// reaches reports whether a recorded reference reaches the definition at key:
// its pointer or anything inside it, its identifier, or one of its anchors.
func (w *walk) reaches(key string, d def) bool {
	for _, r := range w.refs {
		switch {
		case r.target == key, strings.HasPrefix(r.target, key+"/"):
			return true
		case d.id != "" && stripFragment(r.target) == d.id:
			return true
		case slices.Contains(d.anchors, r.target):
			return true
		}
	}
	return false
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
