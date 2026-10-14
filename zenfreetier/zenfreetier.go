// Package zenfreetier is a Caddy handler that rewrites requests so the
// OpenCode Zen free-tier gate accepts them. It ports zen-free-tier-headers.ts.
package zenfreetier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

const (
	defaultUserAgent = "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	defaultClient    = "desktop"

	stubDescription = "Gateway-validation stub, NOT IMPLEMENTED. NEVER call this tool - any call fails; use a real registered tool instead."
	stubParameters  = `{"type":"object","properties":{"command":{"type":"string","description":"The command to execute"},"path":{"type":"string","description":"The path to read"},"timeout":{"type":"number","description":"Optional timeout in milliseconds"},"description":{"type":"string","description":"Clear, concise description of what this command does in 5-10 words."}},"required":[],"additionalProperties":false}`
)

// coreToolNames are the tool names the gateway counts. Both must be present.
var coreToolNames = []string{"bash", "read"}

// sessionIDPattern is the OpenCode session id shape: ses_ plus 26 base62 characters.
var sessionIDPattern = regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`)

func init() {
	caddy.RegisterModule(ZenFreeTier{})
	httpcaddyfile.RegisterHandlerDirective("zen_free_tier", parseCaddyfile)
}

// ZenFreeTier rewrites every request that passes through it.
type ZenFreeTier struct {
	// SessionID is sent in the session headers on every request. Required.
	SessionID string `json:"session_id,omitempty"`
	// UserAgent replaces the default OpenCode User-Agent.
	UserAgent string `json:"user_agent,omitempty"`
	// NoNormalizeBody sends JSON bodies unchanged: no stream forcing, no stubs.
	NoNormalizeBody bool `json:"no_normalize_body,omitempty"`
	// NoInjectTools disables adding the bash and read stub tools.
	NoInjectTools bool `json:"no_inject_tools,omitempty"`
}

var (
	_ caddy.Module                = (*ZenFreeTier)(nil)
	_ caddy.Provisioner           = (*ZenFreeTier)(nil)
	_ caddyhttp.MiddlewareHandler = (*ZenFreeTier)(nil)
	_ caddyfile.Unmarshaler       = (*ZenFreeTier)(nil)
)

// CaddyModule returns the module information.
func (ZenFreeTier) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.zen_free_tier",
		New: func() caddy.Module { return new(ZenFreeTier) },
	}
}

// Provision validates the config. A bad session id stops Caddy at startup.
// The id itself is never printed.
func (m *ZenFreeTier) Provision(caddy.Context) error {
	if !sessionIDPattern.MatchString(m.SessionID) {
		return fmt.Errorf("zen_free_tier: session_id must be ses_ followed by 26 base62 characters (got %d characters)", len(m.SessionID))
	}
	if m.UserAgent == "" {
		m.UserAgent = defaultUserAgent
	}
	return nil
}

// ServeHTTP patches the headers and body, then passes the request on.
func (m *ZenFreeTier) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	m.patchHeaders(r.Header)
	if err := m.patchBody(r); err != nil {
		return caddyhttp.Error(http.StatusBadRequest, fmt.Errorf("zen_free_tier: reading request body: %w", err))
	}
	return next.ServeHTTP(w, r)
}

// patchHeaders pins the session id and sets the OpenCode client headers.
// A configured id always replaces the caller's, as in the TS file.
func (m *ZenFreeTier) patchHeaders(h http.Header) {
	h.Set("x-session-id", m.SessionID)
	h.Set("x-session-affinity", m.SessionID)
	h.Set("x-opencode-session", m.SessionID)
	if h.Get("x-opencode-client") == "" {
		h.Set("x-opencode-client", defaultClient)
	}
	h.Set("User-Agent", m.UserAgent)
}

// patchBody reads the body and rewrites it when it is a JSON object. The body
// is always restored so the proxy can send it.
func (m *ZenFreeTier) patchBody(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	if out, changed := m.normalizeBody(r.URL.Path, raw); changed {
		raw = out
	}
	if len(raw) == 0 {
		r.Body = http.NoBody
		return nil
	}
	// The length may have changed. A stale Content-Length would corrupt the request.
	r.Header.Del("Content-Length")
	r.TransferEncoding = nil
	r.ContentLength = int64(len(raw))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return nil
}

// normalizeBody ports normalizeZenBody from zen-free-tier-headers.ts. It
// returns the new body and whether it changed. On any doubt it returns the
// original bytes.
func (m *ZenFreeTier) normalizeBody(path string, raw []byte) ([]byte, bool) {
	if m.NoNormalizeBody {
		return raw, false
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return raw, false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return raw, false
	}

	names := toolNames(doc["tools"])
	present := 0
	for _, core := range coreToolNames {
		if slices.Contains(names, core) {
			present++
		}
	}

	// Enough core names already, injection disabled, or a forced tool call
	// (the model would surely call a stub): only force streaming.
	if m.NoInjectTools || present >= len(coreToolNames) || forcedToolChoice(doc["tool_choice"]) {
		if isTrue(doc["stream"]) {
			return raw, false
		}
		doc["stream"] = json.RawMessage("true")
		return encode(doc, raw)
	}

	// Append a stub for each missing core tool.
	tools := toolList(doc["tools"])
	for _, name := range coreToolNames {
		if slices.Contains(names, name) {
			continue
		}
		stub, ok := buildStub(path, name)
		if !ok {
			return raw, false // unknown endpoint: leave the body alone
		}
		tools = append(tools, stub)
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return raw, false
	}
	doc["tools"] = toolsJSON
	doc["stream"] = json.RawMessage("true")
	return encode(doc, raw)
}

// toolList returns the entries of a `tools` array, or nil when the value is
// absent or not an array.
func toolList(v json.RawMessage) []json.RawMessage {
	var tools []json.RawMessage
	if json.Unmarshal(v, &tools) != nil {
		return nil
	}
	return tools
}

// toolNames returns tool names in both the Responses shape ({"name": ...})
// and the chat-completions shape ({"function": {"name": ...}}).
func toolNames(v json.RawMessage) []string {
	var names []string
	for _, t := range toolList(v) {
		var entry struct {
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(t, &entry) != nil {
			continue
		}
		name := entry.Name
		if name == "" && entry.Function != nil {
			name = entry.Function.Name
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// forcedToolChoice reports whether tool_choice forces a tool call.
func forcedToolChoice(v json.RawMessage) bool {
	if len(v) == 0 {
		return false
	}
	var choice any
	if json.Unmarshal(v, &choice) != nil {
		return false
	}
	switch c := choice.(type) {
	case string:
		return c == "required" || c == "any"
	case map[string]any:
		t, _ := c["type"].(string)
		return t == "required" || t == "any" || t == "tool"
	}
	return false
}

// buildStub returns a stub tool in the envelope the endpoint expects: Responses,
// chat completions, or Anthropic Messages. It returns false for other paths.
func buildStub(path, name string) (json.RawMessage, bool) {
	params := json.RawMessage(stubParameters)
	var stub any
	switch {
	case strings.Contains(path, "/responses"):
		stub = map[string]any{"type": "function", "name": name, "description": stubDescription, "parameters": params}
	case strings.Contains(path, "/chat/completions"):
		stub = map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name, "description": stubDescription, "parameters": params},
		}
	case strings.HasSuffix(path, "/messages"):
		// Anthropic Messages API: top-level name, schema under input_schema.
		stub = map[string]any{"name": name, "description": stubDescription, "input_schema": params}
	default:
		return nil, false
	}
	out, err := json.Marshal(stub)
	return out, err == nil
}

// isTrue reports whether a raw JSON value is the literal true.
func isTrue(v json.RawMessage) bool {
	return string(v) == "true"
}

// encode marshals the document. On failure it returns the original bytes.
func encode(doc map[string]json.RawMessage, fallback []byte) ([]byte, bool) {
	out, err := json.Marshal(doc)
	if err != nil {
		return fallback, false
	}
	return out, true
}

// UnmarshalCaddyfile parses:
//
//	zen_free_tier {
//		session_id {$ZEN_SESSION_ID}
//		user_agent "..."        # optional
//		no_inject_tools         # optional
//		no_normalize_body       # optional
//	}
func (m *ZenFreeTier) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume the directive name
	for d.NextBlock(0) {
		switch d.Val() {
		case "session_id":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.SessionID = d.Val()
		case "user_agent":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.UserAgent = d.Val()
		case "no_inject_tools":
			m.NoInjectTools = true
		case "no_normalize_body":
			m.NoNormalizeBody = true
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m ZenFreeTier
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &m, nil
}
