// Package remote is how the tool-server adapter registers its tools. Only
// packages under tool can import it, so the source kind of a tool's results
// follows from how the tool was registered and is never declared by the tool.
package remote

import (
	"context"
	"encoding/json"
)

// Call runs one call on the tool server with arguments already validated
// against the tool's schema.
type Call func(ctx context.Context, args json.RawMessage) (string, error)

// Register adds a tool served by a tool server to reg, which must be a
// *tool.Registry. Package tool sets it.
// needsApproval makes every call of the tool wait for approval.
var Register func(reg any, name, description string, schema json.RawMessage, needsApproval bool, call Call) error
