// Package source fetches, unpacks and detects install media referenced by
// a local path or an http(s) URL, presenting each as a read-only io/fs.FS.
package source

import (
	"net/http"
	"os"
	"strings"
)

// Credential is HTTP Basic auth for one host, scoped to https requests only.
type Credential struct {
	Host, Username, Password string
}

// Credentials is a set of host-matched credentials, tried in order.
type Credentials []Credential

// apply attaches Basic auth to req if it is an https request to a host
// matching one of c's credentials. A plain http request, or a host with no
// matching credential, is left unauthenticated. Never log or error on req
// after this call in a way that could expose the attached credentials.
func (c Credentials) apply(req *http.Request) {
	if req.URL.Scheme != "https" {
		return
	}
	host := req.URL.Hostname()
	for _, cred := range c {
		if strings.EqualFold(cred.Host, host) {
			req.SetBasicAuth(cred.Username, cred.Password)
			return
		}
	}
}

// ExpandEnv returns os.Getenv(NAME) when v is exactly "${NAME}"; otherwise
// it returns v unchanged. Config calls this when building Credentials from
// a literal or an environment reference.
func ExpandEnv(v string) string {
	if len(v) < 4 || !strings.HasPrefix(v, "${") || !strings.HasSuffix(v, "}") {
		return v
	}
	name := v[2 : len(v)-1]
	if !isEnvName(name) {
		return v
	}
	return os.Getenv(name)
}

// isEnvName reports whether name matches ^[A-Za-z_][A-Za-z0-9_]*$.
func isEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
