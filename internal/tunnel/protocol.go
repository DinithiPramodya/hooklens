// Package tunnel defines the wire protocol spoken between the hooklens server
// and a CLI holding a tunnel open, and the server half of that conversation.
//
// The transport is one WebSocket carrying JSON. See
// docs/learn/18-websockets.md for why a WebSocket and not SSE, and
// docs/learn/17-nat-and-firewalls.md for why the connection has to be opened
// by the client.
package tunnel

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the version a client announces in its hello.
//
// Checked, not merely recorded. A CLI installed months ago and never updated
// is the normal case for a developer tool, so the server has to be able to
// say "your client is too old" in words rather than failing somewhere deeper
// with a confusing decode error.
const ProtocolVersion = 1

// Type names a frame. Frames are JSON objects and the type is the only field
// that must be present on every one.
type Type string

const (
	// Client to server.
	TypeHello    Type = "hello"
	TypeResponse Type = "response"

	// Server to client.
	TypeHelloOK Type = "hello_ok"
	TypeRequest Type = "request"
	TypeClose   Type = "close"
)

// Envelope is the outer shape of every frame.
//
// The payload stays as RawMessage so decoding happens in two stages: read the
// type, then unmarshal the payload into the struct that type implies. The
// alternative -- one flat struct with the union of every frame's fields -- is
// tempting because it is less code, and it is wrong in a specific way: it
// cannot distinguish "this field was absent" from "this field was zero", so a
// response frame claiming status 0 and one that forgot to send a status
// decode identically.
type Envelope struct {
	Type    Type            `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hello is the first frame a client sends. It authenticates the connection.
//
// Authentication is in-band -- in this frame -- rather than in an
// Authorization header on the upgrade request. See the note for the tradeoff;
// the short version is that it keeps every failure a protocol-level close
// frame with a printable reason, instead of some failures being an HTTP status
// the client has to interpret differently.
type Hello struct {
	Slug    string `json:"slug"`
	Token   string `json:"token"`
	Version int    `json:"version"`
}

// Header is one HTTP header as it crosses the tunnel.
//
// Its own type rather than reusing capture.Header, for the reason the store
// keeps its own headerJSON: this shape is a CONTRACT WITH A SEPARATELY
// VERSIONED PROGRAM. A CLI installed months ago is still speaking it, so
// renaming a field on an internal domain type must not silently change the
// wire. A four-line struct is a cheap price for the two being able to move
// independently.
//
// A slice of pairs, not a map, for the reason established in
// docs/learn/07-storing-a-request.md: header names repeat and their order is
// evidence.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Request is a captured webhook being handed to the CLI for forwarding.
type Request struct {
	// ReqID correlates this request with its response. Assigned by the
	// server; a client must echo it back unchanged.
	ReqID   string   `json:"req_id"`
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Query   string   `json:"query"`
	Headers []Header `json:"headers"`
	BodyB64 string   `json:"body_b64"`
}

// Response is what the local app said, relayed back.
type Response struct {
	ReqID   string   `json:"req_id"`
	Status  int      `json:"status"`
	Headers []Header `json:"headers"`
	BodyB64 string   `json:"body_b64"`
	// Error is set when the CLI could not reach the local app at all --
	// connection refused, DNS failure, its own timeout. Distinct from a
	// response with a 5xx status, which means the local app was reached and
	// answered badly. Conflating them would tell a developer their app is
	// broken when in fact it is not running.
	Error string `json:"error,omitempty"`
}

// HelloOK accepts a hello.
type HelloOK struct {
	Slug      string `json:"slug"`
	PublicURL string `json:"public_url"`
	// PingInterval tells the client how often the server will ping, so the
	// two ends cannot disagree about liveness. Sent by the server rather than
	// hardcoded in the client, because an old CLI must not be the thing that
	// pins this value.
	PingSeconds int `json:"ping_seconds"`
}

// Close is the server's last word before hanging up, carrying a reason the
// CLI can print verbatim.
//
// This exists because "the connection closed" is the least useful thing a
// developer tool can say. Every close path in this package supplies a reason,
// and CloseReason names the ones the client is expected to handle.
type Close struct {
	Reason string `json:"reason"`
	Code   string `json:"code"`
}

// Close codes. Strings rather than integers: they appear in logs and in CLI
// output, and "unauthorized" reads better than 4001 in both.
const (
	CodeUnauthorized   = "unauthorized"
	CodeVersion        = "unsupported_version"
	CodeMalformed      = "malformed_frame"
	CodeHandshake      = "handshake_timeout"
	CodeReplaced       = "replaced"
	CodeServerShutdown = "server_shutdown"
)

// CloseError is a close frame received by a client, as a Go error.
//
// A typed error rather than a formatted string because the caller has to make
// a decision on it: a rejected token will never start working, so retrying it
// is a busy-wait that also looks like a credential attack. Reason is carried
// verbatim so the CLI can print the server's own sentence.
type CloseError struct {
	Code   string
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "connection closed: " + e.Code
}

// Permanent reports whether retrying could ever succeed.
//
// The listed codes describe the CLIENT being wrong, and nothing about waiting
// changes that. Everything else -- shutdown, replacement, a malformed frame,
// a dropped socket -- is either transient or fixed by reconnecting, so the
// default is to retry. Getting this backwards in either direction is
// expensive: retry a permanent failure and you have a hot loop against an
// auth endpoint, treat a transient one as permanent and the tool gives up on
// a server that was restarting.
func (e *CloseError) Permanent() bool {
	switch e.Code {
	case CodeUnauthorized, CodeVersion:
		return true
	default:
		return false
	}
}

// Encode marshals a frame into an envelope ready to write as one WebSocket
// message.
func Encode(t Type, payload any) ([]byte, error) {
	env := Envelope{Type: t}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode %s payload: %w", t, err)
		}
		env.Payload = raw
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode %s envelope: %w", t, err)
	}
	return b, nil
}

// DecodeEnvelope reads only the outer frame, leaving the payload untouched.
func DecodeEnvelope(b []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, fmt.Errorf("decode envelope: missing type")
	}
	return env, nil
}

// DecodePayload unmarshals an envelope's payload into v.
func DecodePayload(env Envelope, v any) error {
	if len(env.Payload) == 0 {
		return fmt.Errorf("decode %s: empty payload", env.Type)
	}
	if err := json.Unmarshal(env.Payload, v); err != nil {
		return fmt.Errorf("decode %s payload: %w", env.Type, err)
	}
	return nil
}
