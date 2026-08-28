// Package source fetches, unpacks and detects install media referenced by
// a local path or an http(s) URL, presenting each as a read-only io/fs.FS.
package source

import (
	"net/http"
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
