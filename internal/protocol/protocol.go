// Package protocol defines the request shape shared by the CLI and daemon.
// Responses are open-ended JSON objects built by handlers.
package protocol

import "encoding/json"

type Request struct {
	Op         string          `json:"op"`
	Kind       string          `json:"kind,omitempty"`
	Pkg        string          `json:"pkg,omitempty"`
	Sym        string          `json:"sym,omitempty"`
	Body       string          `json:"body,omitempty"`
	To         string          `json:"to,omitempty"`
	Name       string          `json:"name,omitempty"`
	Type       string          `json:"type,omitempty"`
	Default    string          `json:"default,omitempty"`
	Ops        json.RawMessage `json:"ops,omitempty"`
	Generation int64           `json:"generation,omitempty"`
	DryRun     bool            `json:"dry_run,omitempty"`
	Offset     int             `json:"offset,omitempty"`
}
