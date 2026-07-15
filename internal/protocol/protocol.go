// Package protocol defines the request shape shared by the CLI and daemon.
// Responses are open-ended JSON objects built by handlers.
package protocol

type Request struct {
	Op   string `json:"op"`
	Pkg  string `json:"pkg,omitempty"`
	Sym  string `json:"sym,omitempty"`
	Body string `json:"body,omitempty"`
	To   string `json:"to,omitempty"`
}
