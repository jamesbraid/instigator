package source

import (
	"net/http"
	"testing"
)

func TestCredentialsApplyHTTPSOnlyByHost(t *testing.T) {
	creds := Credentials{{Host: "forge.example", Username: "ci", Password: "secret"}}

	https, _ := http.NewRequest("GET", "https://forge.example/x", nil)
	creds.apply(https)
	if u, p, ok := https.BasicAuth(); !ok || u != "ci" || p != "secret" {
		t.Errorf("https basic auth = %q/%q ok=%v, want ci/secret true", u, p, ok)
	}

	plain, _ := http.NewRequest("GET", "http://forge.example/x", nil)
	creds.apply(plain)
	if _, _, ok := plain.BasicAuth(); ok {
		t.Error("basic auth attached over plain http; must be https-only")
	}

	other, _ := http.NewRequest("GET", "https://elsewhere.example/x", nil)
	creds.apply(other)
	if _, _, ok := other.BasicAuth(); ok {
		t.Error("basic auth attached to a non-matching host")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("FJ_TOKEN", "tok123")
	if got := ExpandEnv("${FJ_TOKEN}"); got != "tok123" {
		t.Errorf("ExpandEnv(${FJ_TOKEN}) = %q, want tok123", got)
	}
	if got := ExpandEnv("literal"); got != "literal" {
		t.Errorf("ExpandEnv(literal) = %q, want literal", got)
	}
}
