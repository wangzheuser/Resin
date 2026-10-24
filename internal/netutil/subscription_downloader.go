package netutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
	xproxy "golang.org/x/net/proxy"
)

type DownloadAttempt struct {
	Route      string `json:"route"`
	NodeHash   string `json:"node_hash,omitempty"`
	Stage      string `json:"stage"`
	DurationMS int64  `json:"duration_ms"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Error      string `json:"error,omitempty"`
}

type SubscriptionFetchError struct {
	Kind       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *SubscriptionFetchError) Error() string { return e.Message }

type DownloadRoute struct {
	Hash     node.Hash
	EgressIP netip.Addr
}
type downloadPreference struct {
	route   string
	expires time.Time
}

// SubscriptionDownloader owns only subscription traffic. GeoIP keeps its
// existing Downloader contract and routing behavior.
type SubscriptionDownloader struct {
	Timeout          func() time.Duration
	UserAgent        func() string
	ExternalProxyURL string
	Pick             func(string, string, map[node.Hash]struct{}, map[netip.Addr]struct{}) (DownloadRoute, error)
	Fetch            func(context.Context, node.Hash, string) ([]byte, error)
	ValidateBody     func([]byte) error
	mu               sync.Mutex
	preferences      map[string]downloadPreference
	generation       uint64
}

func ValidateSubscriptionProxy(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("subscription proxy must be a valid proxy URL without path, query or fragment")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	}
	return errors.New("subscription proxy scheme must be http, https, socks5 or socks5h")
}

func configureSubscriptionProxyTransport(tr *http.Transport, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
		return nil
	case "socks5", "socks5h":
		dialer, err := xproxy.FromURL(u, &net.Dialer{})
		if err != nil {
			return err
		}
		contextDialer, ok := dialer.(xproxy.ContextDialer)
		if !ok {
			return errors.New("subscription SOCKS proxy does not support context dialing")
		}
		tr.Proxy = nil
		tr.DialContext = contextDialer.DialContext
		return nil
	default:
		return errors.New("unsupported subscription proxy scheme")
	}
}

func (d *SubscriptionDownloader) Invalidate() {
	d.mu.Lock()
	d.preferences = nil
	d.generation++
	d.mu.Unlock()
}

func (d *SubscriptionDownloader) preferred(host string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.preferences[host]
	if time.Now().Before(p.expires) {
		return p.route
	}
	delete(d.preferences, host)
	return ""
}

func (d *SubscriptionDownloader) remember(host, route string, generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if generation != d.generation {
		return
	}
	if d.preferences == nil {
		d.preferences = make(map[string]downloadPreference)
	}
	now := time.Now()
	for k, v := range d.preferences {
		if !now.Before(v.expires) {
			delete(d.preferences, k)
		}
	}
	if len(d.preferences) >= 512 {
		var oldest string
		var expiry time.Time
		for k, v := range d.preferences {
			if expiry.IsZero() || v.expires.Before(expiry) {
				oldest, expiry = k, v.expires
			}
		}
		delete(d.preferences, oldest)
	}
	d.preferences[host] = downloadPreference{route, now.Add(10 * time.Minute)}
}

// DownloadSubscription returns a bounded, credential-free trace, including
// unsuccessful route selection. Explicit modes never fall back to another mode.
func (d *SubscriptionDownloader) DownloadSubscription(ctx context.Context, target, mode, platformID string) ([]byte, []DownloadAttempt, error) {
	d.mu.Lock()
	generation := d.generation
	d.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := 30 * time.Second
	if d.Timeout != nil && d.Timeout() > 0 {
		timeout = d.Timeout()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*timeout)
	defer cancel()
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, nil, &SubscriptionFetchError{Kind: "configuration", Message: "invalid subscription URL"}
	}
	if mode == "" {
		mode = "auto"
	}
	routes := []string{mode}
	if mode == "auto" {
		routes = []string{"direct"}
		if d.ExternalProxyURL != "" {
			routes = append(routes, "external_proxy")
		}
		routes = append(routes, "platform", "platform")
		if preferred := d.preferred(u.Hostname()); preferred != "" {
			ordered := make([]string, 0, len(routes))
			for _, r := range routes {
				if r == preferred {
					ordered = append(ordered, r)
				}
			}
			for _, r := range routes {
				if r != preferred {
					ordered = append(ordered, r)
				}
			}
			routes = ordered
		}
	} else if mode == "platform" {
		routes = []string{"platform", "platform"}
	}
	attempts := make([]DownloadAttempt, 0, len(routes))
	excluded := make(map[node.Hash]struct{})
	ips := make(map[netip.Addr]struct{})
	var last *SubscriptionFetchError
	for _, route := range routes {
		if ctx.Err() != nil {
			last = ClassifySubscriptionError(ctx.Err())
			break
		}
		started := time.Now()
		attempt := DownloadAttempt{Route: route}
		attemptCtx, stop := context.WithTimeout(ctx, timeout)
		var body []byte
		switch route {
		case "direct", "external_proxy":
			if route == "external_proxy" && (d.ExternalProxyURL == "" || ValidateSubscriptionProxy(d.ExternalProxyURL) != nil) {
				err = &SubscriptionFetchError{Kind: "configuration", Message: "external subscription proxy is not configured correctly"}
				break
			}
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = nil
			if route == "external_proxy" {
				if proxyErr := configureSubscriptionProxyTransport(tr, d.ExternalProxyURL); proxyErr != nil {
					err = &SubscriptionFetchError{Kind: "configuration", Message: "external subscription proxy is not configured correctly"}
					break
				}
			}
			direct := NewDirectDownloader(func() time.Duration { return timeout }, func() string {
				if d.UserAgent != nil {
					return d.UserAgent()
				}
				return "clash.meta"
			})
			direct.Client.Transport = tr
			body, err = direct.Download(attemptCtx, target)
			tr.CloseIdleConnections()
		case "platform":
			if d.Pick == nil || d.Fetch == nil {
				err = &SubscriptionFetchError{Kind: "no_route", Message: "subscription download has no available internal route", Retryable: true}
				break
			}
			var selected DownloadRoute
			selected, err = d.Pick(target, platformID, excluded, ips)
			if err != nil {
				err = &SubscriptionFetchError{Kind: "no_route", Message: "subscription download platform has no eligible route", Retryable: true}
				break
			}
			if _, exists := excluded[selected.Hash]; exists {
				err = &SubscriptionFetchError{Kind: "no_route", Message: "subscription download has exhausted distinct routes", Retryable: true}
				break
			}
			excluded[selected.Hash] = struct{}{}
			if selected.EgressIP.IsValid() {
				ips[selected.EgressIP] = struct{}{}
			}
			attempt.NodeHash = selected.Hash.Hex()
			body, err = d.Fetch(attemptCtx, selected.Hash, target)
		default:
			err = &SubscriptionFetchError{Kind: "configuration", Message: "invalid subscription download mode"}
		}
		stop()
		attempt.DurationMS = time.Since(started).Milliseconds()
		if err == nil && d.ValidateBody != nil {
			if validateErr := d.ValidateBody(body); validateErr != nil {
				err = &SubscriptionFetchError{Kind: "parse", Message: "subscription content could not be parsed", Retryable: true}
			}
		}
		if err == nil {
			attempt.Stage = "complete"
			attempt.HTTPStatus = 200
			attempts = append(attempts, attempt)
			if mode == "auto" {
				d.remember(u.Hostname(), route, generation)
			}
			return body, attempts, nil
		}
		failure := ClassifySubscriptionError(err)
		attempt.Stage = failure.Kind
		attempt.Error = failure.Message
		var status *HTTPStatusError
		if errors.As(err, &status) {
			attempt.HTTPStatus = status.StatusCode
		}
		attempts = append(attempts, attempt)
		// Keep an actual transport/upstream cause when later selection finds no route.
		if last == nil || failure.Kind != "no_route" {
			last = failure
		}
		if !failure.Retryable || failure.RetryAfter > 0 {
			break
		}
	}
	if last == nil {
		last = &SubscriptionFetchError{Kind: "no_route", Message: "no subscription download route", Retryable: true}
	}
	return nil, attempts, last
}

func ClassifySubscriptionError(err error) *SubscriptionFetchError {
	var known *SubscriptionFetchError
	if errors.As(err, &known) {
		return known
	}
	e := &SubscriptionFetchError{Kind: "connection", Message: RedactErrorMessage(err.Error()), Retryable: true}
	var status *HTTPStatusError
	var dns *net.DNSError
	var network net.Error
	var tooLarge *ResponseTooLargeError
	switch {
	case errors.Is(err, context.Canceled):
		e.Kind = "cancelled"
		e.Retryable = false
	case errors.Is(err, context.DeadlineExceeded):
		e.Kind = "timeout"
	case errors.As(err, &status):
		e.Kind = "http"
		e.Retryable = shouldRetryHTTPStatusCode(status.StatusCode)
		e.RetryAfter = status.RetryAfter
		// Authentication/authorization errors need source correction, not rapid retries.
		if status.StatusCode == 403 {
			e.Retryable = false
		}
	case errors.As(err, &dns):
		e.Kind = "dns"
	case errors.As(err, &network) && network.Timeout():
		e.Kind = "timeout"
	case errors.As(err, &tooLarge):
		e.Kind = "content_too_large"
		e.Retryable = false
	case strings.Contains(strings.ToLower(err.Error()), "tls") || strings.Contains(strings.ToLower(err.Error()), "certificate"):
		e.Kind = "tls"
	}
	// Proxy credentials may appear outside URLs in SOCKS errors. Do not retain
	// arbitrary transport text; the structured stage is enough to diagnose it.
	if e.Kind != "http" {
		e.Message = fmt.Sprintf("subscription download %s failure", e.Kind)
	}
	return e
}
