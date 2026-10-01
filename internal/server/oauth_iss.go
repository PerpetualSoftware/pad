package server

import (
	"net/http"
	"net/url"
	"strings"
)

// RFC 9207 (TASK-3321 U0a): every authorization response that redirects back
// to the client carries iss=<issuer>, success and error alike, so a client
// talking to several authorization servers can tell which one answered (the
// mix-up attack). fosite v0.49 does not add it, so the redirects fosite
// writes go through issRedirectWriter, which appends it to the Location
// fosite built. Only fosite's authorize writes are wrapped; this server's own
// redirects (to /login, the consent page) are not authorization responses.
//
// The value is the issuer exactly as the metadata document publishes it:
// clients compare it as a string, without normalising.

type issRedirectWriter struct {
	http.ResponseWriter
	issuer string
	done   bool
}

// authorizeResponseWriter wraps w for one fosite WriteAuthorizeResponse or
// WriteAuthorizeError call. With no configured issuer (OAuth unavailable)
// it returns w unchanged, matching the metadata, which then advertises
// nothing.
func (s *Server) authorizeResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	issuer := s.authServerIssuerURL()
	if issuer == "" {
		return w
	}
	return &issRedirectWriter{ResponseWriter: w, issuer: issuer}
}

func (w *issRedirectWriter) WriteHeader(code int) {
	if !w.done {
		w.done = true
		if code >= 300 && code < 400 {
			if loc := w.Header().Get("Location"); loc != "" {
				w.Header().Set("Location", withIssParam(loc, w.issuer))
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *issRedirectWriter) Write(b []byte) (int, error) {
	if !w.done {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// withIssParam adds iss to the part of the redirect that carries the response
// parameters: the fragment when fosite answered in fragment mode, otherwise
// the query. An existing iss is replaced, never duplicated. A Location that
// does not parse is returned unchanged rather than mangled.
func withIssParam(loc, issuer string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if frag := u.EscapedFragment(); frag != "" && (strings.Contains(frag, "code=") || strings.Contains(frag, "error=")) {
		f, err := url.ParseQuery(frag)
		if err != nil {
			return loc
		}
		f.Set("iss", issuer)
		// url.URL.Fragment holds the DECODED form, so assigning an encoded
		// query to it would escape it twice; rebuild the fragment by hand.
		u.Fragment, u.RawFragment = "", ""
		return u.String() + "#" + f.Encode()
	}
	q := u.Query()
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	return u.String()
}
