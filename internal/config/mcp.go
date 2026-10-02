package config

import (
	"fmt"
	"net/url"
	"strings"
)

// MCP public addressing (PLAN-2310 DR-3). Every URL an MCP client or an
// OAuth flow sees is derived from configuration here, never from a
// request's Host: on a LAN box the Host header is chosen by whoever sent the
// request (DNS rebinding), and a guessed "https://"+Host on an http box
// points clients at a URL that does not exist.
//
// All three inputs are startup-time configuration (environment, the toml
// file, or a flag). None is a runtime-writable setting, so a value derived
// from them, such as the OAuth audience fixed at construction, cannot go
// stale while the process runs (PLAN-2310 DR-4).

// MCPEndpoints is the resolved addressing, with the reason for each value
// that could not be used.
type MCPEndpoints struct {
	// Origin is the deployment's public origin: cfg.URL (PAD_URL, toml
	// `url`, or --url), else PUBLIC_URL. Empty when neither is set or the
	// value is unusable; OriginErr then says why.
	Origin    string
	OriginVar string // the variable the origin came from, for messages
	OriginErr string

	// ResourceURL is the MCP URL clients connect to, and the OAuth
	// audience: PAD_MCP_PUBLIC_URL in its historical spelling
	// (historicalOverride), else Origin + "/mcp".
	ResourceURL    string
	ResourceURLErr string

	// ChatGPTResourceURL is the ChatGPT catalog's MCP URL and its OWN OAuth
	// audience (TASK-3321 U2a, ruling (i)): the /mcp resource with
	// "/chatgpt" appended to its MCP path, so https://mcp.getpad.dev (whose
	// root the cloud proxy maps to /mcp) gives https://mcp.getpad.dev/mcp/chatgpt
	// and https://pad.example/mcp gives https://pad.example/mcp/chatgpt. Empty
	// when ResourceURL is. A token is bound to exactly one of the two.
	ChatGPTResourceURL string

	// AuthServerURL is the OAuth issuer: PAD_AUTH_SERVER_URL in its
	// historical spelling (historicalOverride), else Origin.
	AuthServerURL    string
	AuthServerURLErr string
}

// Usable reports whether MCP can be addressed at all: an origin and the
// two URLs derived from it all resolved.
func (e MCPEndpoints) Usable() bool {
	return e.Origin != "" && e.ResourceURL != "" && e.AuthServerURL != ""
}

// HTTPS reports whether the auth-server URL is https, which is what makes
// OAuth available (PLAN-2310 DR-4, Dave's ruling: http deployments are
// PAT-only).
func (e MCPEndpoints) HTTPS() bool {
	if !e.Usable() {
		return false
	}
	// Parsed rather than prefix-matched: an explicitly set
	// PAD_AUTH_SERVER_URL keeps its historical spelling, scheme case
	// included (historicalOverride).
	u, err := url.Parse(strings.TrimSpace(e.AuthServerURL))
	return err == nil && strings.EqualFold(u.Scheme, "https")
}

// Problems lists every configured value that could not be used, as
// "VARIABLE: reason" lines for a startup warning. An unset origin is not a
// problem; it is the default, and MCP simply stays unavailable.
func (e MCPEndpoints) Problems() []string {
	var out []string
	for _, p := range []string{e.OriginErr, e.ResourceURLErr, e.AuthServerURLErr} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ResolveMCPEndpoints derives the MCP addressing from the configuration. It
// never fails: PAD_URL also tells the CLI where the server is, so an install
// that set it for that purpose must keep booting whatever it contains. An
// unusable value leaves the matching field empty with a reason instead.
func (c *Config) ResolveMCPEndpoints() MCPEndpoints {
	var e MCPEndpoints

	raw, name := c.URL, "PAD_URL"
	if raw == "" {
		raw, name = c.PublicURL, "PUBLIC_URL"
	}
	if raw != "" {
		e.OriginVar = name
		if origin, err := parseMCPURL(raw, false); err != nil {
			e.OriginErr = fmt.Sprintf("%s %q is not usable as the public origin: %v", name, raw, err)
		} else {
			e.Origin = origin
		}
	}

	if c.MCPPublicURL != "" {
		if _, err := parseMCPURL(c.MCPPublicURL, true); err != nil {
			e.ResourceURLErr = fmt.Sprintf("PAD_MCP_PUBLIC_URL %q is not usable: %v", c.MCPPublicURL, err)
		} else {
			e.ResourceURL = historicalOverride(c.MCPPublicURL)
		}
	} else if e.Origin != "" {
		e.ResourceURL = e.Origin + "/mcp"
	}

	e.ChatGPTResourceURL = chatGPTResourceURL(e.ResourceURL)

	if c.AuthServerURL != "" {
		if _, err := parseMCPURL(c.AuthServerURL, true); err != nil {
			e.AuthServerURLErr = fmt.Sprintf("PAD_AUTH_SERVER_URL %q is not usable: %v", c.AuthServerURL, err)
		} else {
			e.AuthServerURL = historicalOverride(c.AuthServerURL)
		}
	} else if e.Origin != "" {
		e.AuthServerURL = e.Origin
	}

	// An override without an origin still leaves MCP unavailable, because
	// the other value falls back to the origin. Usable() is the gate.
	return e
}

// chatGPTResourceURL derives the ChatGPT catalog's resource from the /mcp
// one: its path, or "/mcp" when it is the bare host the cloud proxy maps
// to /mcp, followed by "/chatgpt", which is where the catalog is mounted.
func chatGPTResourceURL(resource string) string {
	if resource == "" {
		return ""
	}
	u, err := url.Parse(resource)
	if err != nil || u.Host == "" {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/mcp"
	}
	return u.Scheme + "://" + u.Host + path + "/chatgpt"
}

// historicalOverride is the spelling an explicitly set PAD_MCP_PUBLIC_URL or
// PAD_AUTH_SERVER_URL has always been used in: the value with its trailing
// slashes trimmed, and nothing else rewritten. It is validated like any
// other value, but not canonicalised, because the MCP URL is the OAuth
// audience every issued token is bound to, compared byte-for-byte: a
// deployment whose value is not canonical (an uppercase host, an explicit
// :443) would otherwise find every existing token refused after an upgrade
// (TASK-2317). Only these two variables predate PLAN-2310; the origin and
// the URLs derived from it are new and take the canonical form.
func historicalOverride(raw string) string {
	return strings.TrimRight(raw, "/")
}

// parseMCPURL accepts an absolute http(s) URL with a host, no user info, no
// query and no fragment, and returns it in one canonical spelling: scheme
// and host lower-cased, a default port (443 for https, 80 for http)
// dropped, and no trailing slash. The canonical form matters because a URL
// derived from the origin is the OAuth audience, compared byte-for-byte.
// The two pre-existing overrides are validated here but keep their own
// spelling (historicalOverride). An origin must also have no path, judged on the escaped path,
// so an encoded slash (%2F) cannot pass as empty. The MCP and auth-server
// URLs may have a path.
func parseMCPURL(raw string, pathAllowed bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("does not parse as a URL")
	}
	scheme := strings.ToLower(u.Scheme)
	path := strings.TrimRight(u.EscapedPath(), "/")
	switch {
	case scheme != "http" && scheme != "https":
		return "", fmt.Errorf("the scheme must be http or https")
	case u.Hostname() == "":
		return "", fmt.Errorf("it has no host")
	case strings.HasSuffix(u.Host, ":"):
		return "", fmt.Errorf("it has an empty port")
	case strings.Contains(u.Hostname(), "%"):
		// A zoned IPv6 address (fe80::1%eth0) names an interface on this
		// machine; no remote client can reach it, and re-spelling it
		// canonically would have to preserve the %25 escape and the zone's
		// case. Refuse it rather than emit a malformed URL.
		return "", fmt.Errorf("a zoned IPv6 address is not a public address")
	case u.User != nil:
		return "", fmt.Errorf("it must not carry user info")
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("it must not have a query")
	case u.Fragment != "":
		return "", fmt.Errorf("it must not have a fragment")
	case !pathAllowed && path != "":
		return "", fmt.Errorf("an origin must not have a path (got %q)", u.EscapedPath())
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6
	}
	if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host += ":" + port
	}
	return scheme + "://" + host + path, nil
}

// parseMCPEnabledEnv reads PAD_MCP_ENABLED: true/1/yes/on and
// false/0/no/off, case-insensitive. Anything else forces nothing (nil); the
// raw value is kept on the config so the server warns about it.
func parseMCPEnabledEnv(v string) *bool {
	t, f := true, false
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return &t
	case "false", "0", "no", "off":
		return &f
	}
	return nil
}
