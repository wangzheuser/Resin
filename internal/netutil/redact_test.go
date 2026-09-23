package netutil

import (
	"strings"
	"testing"
)

func TestRedactURL_RemovesCredentialsQueryAndFragment(t *testing.T) {
	got := RedactURL("https://user:pass@example.com/path-secret?token=secret&target=https%3A%2F%2Fexample.org#fragment")
	if got != "https://example.com" {
		t.Fatalf("RedactURL: got %q", got)
	}
}

func TestRedactErrorMessage_RemovesEmbeddedURLSecrets(t *testing.T) {
	message := `Get "https://user:pass@example.com/path-secret?token=secret": context deadline exceeded`
	got := RedactErrorMessage(message)
	if strings.Contains(got, "secret") || strings.Contains(got, "user:pass") {
		t.Fatalf("redacted message still contains credentials: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Fatalf("redacted message lost URL context: %q", got)
	}
}

func TestHTTPStatusError_ErrorRedactsURL(t *testing.T) {
	err := (&HTTPStatusError{
		StatusCode: 404,
		URL:        "https://example.com/path-secret?token=secret",
	}).Error()
	if strings.Contains(err, "secret") || !strings.Contains(err, "https://example.com") {
		t.Fatalf("HTTPStatusError message: %q", err)
	}
}
