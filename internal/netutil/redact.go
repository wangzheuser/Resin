package netutil

import (
	"net/url"
	"regexp"
	"strings"
)

var urlInErrorPattern = regexp.MustCompile(`https?://[^\s"']+`)

// RedactURL keeps only the scheme and host; subscription credentials can live
// in URL userinfo, paths, queries, or fragments.
func RedactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<redacted-url>"
	}
	return u.Scheme + "://" + u.Host
}

// RedactErrorMessage removes URL credentials from transport errors before the
// message is persisted or written to logs.
func RedactErrorMessage(message string) string {
	return urlInErrorPattern.ReplaceAllStringFunc(message, func(token string) string {
		trailing := ""
		for len(token) > 0 && strings.ContainsRune(".,;:)]}", rune(token[len(token)-1])) {
			trailing = string(token[len(token)-1]) + trailing
			token = token[:len(token)-1]
		}
		return RedactURL(token) + trailing
	})
}
