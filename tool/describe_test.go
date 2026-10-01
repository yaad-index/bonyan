package tool

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// describe keeps a rule only when it is a bare keyword, so a message whose
// text before the colon carries anything else, such as a value, yields no rule.
func TestDescribeKeepsOnlyAKeyword(t *testing.T) {
	for msg, want := range map[string]ValidationError{
		"validating root: validating /properties/a: type: x has type":   {Path: "/properties/a", Rule: "type"},
		"validating root: validating /properties/a: MARKER x is bad: y": {Path: "/properties/a"},
		"validating root: validating /properties/a: no colon MARKER":    {Path: "/properties/a"},
		"validating root: unexpected additional properties [MARKER]":    {Path: "/", Rule: "additionalProperties"},
		"validating root: validating MARKER: type: x":                   {},
		"validating root":                   {},
		"something else entirely MARKER: x": {},
	} {
		got := describe(msg)
		assert.Equal(t, want, *got, msg)
		assert.NotContains(t, got.Error(), "MARKER", msg)
	}
}
