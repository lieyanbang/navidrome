package shared

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
)

const (
	DefaultRequestTimeout = 2500 * time.Millisecond
	DefaultTotalTimeout   = 9 * time.Second
	DefaultResponseLimit  = 1 << 20
	DefaultRequestLimit   = 16 << 10
	retryBackoff          = 200 * time.Millisecond
)

type HTTPDoer interface {
	Send(request host.HTTPRequest) (*host.HTTPResponse, error)
}

type HostHTTP struct{}

func (HostHTTP) Send(request host.HTTPRequest) (*host.HTTPResponse, error) {
	return host.HTTPSend(request)
}

type HTTPClient struct {
	Doer           HTTPDoer
	Deadline       time.Time
	Now            func() time.Time
	Sleep          func(time.Duration)
	MaxBody        int
	MaxRequest     int
	RequestTimeout time.Duration
}

func NewHTTPClient(doer HTTPDoer, deadline time.Time, now func() time.Time, sleep func(time.Duration)) *HTTPClient {
	if doer == nil {
		doer = HostHTTP{}
	}
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	return &HTTPClient{
		Doer:           doer,
		Deadline:       deadline,
		Now:            now,
		Sleep:          sleep,
		MaxBody:        DefaultResponseLimit,
		MaxRequest:     DefaultRequestLimit,
		RequestTimeout: DefaultRequestTimeout,
	}
}

// Send validates the destination, disables implicit redirects, applies a
// per-request deadline, bounds response size, and performs at most one short
// retry for common temporary gateway failures.
func (c *HTTPClient) Send(request host.HTTPRequest, allowedHosts []string) (*host.HTTPResponse, error) {
	parsed, err := ValidateHTTPSURL(request.URL, allowedHosts)
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(request.Method)
	if method != "GET" && method != "POST" {
		return nil, fmt.Errorf("unsupported HTTP method %q", method)
	}
	if len(request.Body) > c.MaxRequest {
		return nil, fmt.Errorf("HTTP request body exceeds %d bytes", c.MaxRequest)
	}
	request.Method = method
	request.NoFollowRedirects = true

	for attempt := 0; attempt < 2; attempt++ {
		remaining := c.Deadline.Sub(c.Now())
		if remaining <= 0 {
			return nil, &RetryError{After: time.Second, Cause: errors.New("lookup deadline exceeded")}
		}
		timeout := c.RequestTimeout
		if remaining < timeout {
			timeout = remaining
		}
		request.TimeoutMs = int32(math.Max(1, float64(timeout.Milliseconds())))

		response, sendErr := c.Doer.Send(request)
		if sendErr != nil {
			return nil, &RetryError{After: time.Second, Cause: sendErr}
		}
		if response == nil {
			return nil, &RetryError{After: time.Second, Cause: errors.New("empty HTTP response")}
		}
		switch {
		case response.StatusCode >= 300 && response.StatusCode < 400:
			return nil, &RedirectError{StatusCode: response.StatusCode}
		case response.StatusCode == 429:
			return nil, &RetryError{After: RetryAfter(response.Headers, c.Now()), Cause: errors.New("provider rate limited the request")}
		case response.StatusCode >= 500:
			if attempt == 0 && parsed.Hostname() != "musicbrainz.org" && response.StatusCode >= 502 && response.StatusCode <= 504 {
				if c.Now().Add(retryBackoff).Before(c.Deadline) {
					c.Sleep(retryBackoff)
					continue
				}
			}
			return nil, &RetryError{After: RetryAfter(response.Headers, c.Now()), Cause: fmt.Errorf("provider returned HTTP %d", response.StatusCode)}
		case response.StatusCode != 200:
			return nil, &StatusError{StatusCode: response.StatusCode}
		case len(response.Body) > c.MaxBody:
			return nil, &BodyTooLargeError{Limit: c.MaxBody, Actual: len(response.Body)}
		default:
			return response, nil
		}
	}
	return nil, &RetryError{After: time.Second, Cause: errors.New("provider retry exhausted")}
}

func ValidateHTTPSURL(raw string, allowedHosts []string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("URL must be HTTPS without user info or fragment")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, errors.New("URL port is not allowed")
	}
	hostname := strings.ToLower(u.Hostname())
	allowed := false
	for _, candidate := range allowedHosts {
		if hostname == strings.ToLower(candidate) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("URL host %q is not allowed", hostname)
	}
	return u, nil
}

type RetryError struct {
	After time.Duration
	Cause error
}

func (e *RetryError) Error() string {
	if e.Cause != nil {
		return "temporary provider failure: " + e.Cause.Error()
	}
	return "temporary provider failure"
}

func (e *RetryError) Unwrap() error {
	return e.Cause
}

type RedirectError struct {
	StatusCode int32
}

func (e *RedirectError) Error() string {
	return fmt.Sprintf("provider redirect %d was blocked", e.StatusCode)
}

type StatusError struct {
	StatusCode int32
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d", e.StatusCode)
}

type BodyTooLargeError struct {
	Limit  int
	Actual int
}

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("provider response exceeds %d bytes", e.Limit)
}

func RetryDelay(err error) time.Duration {
	var retryErr *RetryError
	if errors.As(err, &retryErr) && retryErr.After > 0 {
		return retryErr.After
	}
	return time.Second
}

func RetryAfter(headers map[string]string, now time.Time) time.Duration {
	for key, value := range headers {
		if !strings.EqualFold(key, "Retry-After") {
			continue
		}
		if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			// A non-positive Retry-After is not useful for a provider that is
			// already throttling or unavailable. Fall through to the conservative
			// default instead of reopening the circuit after only one second.
			if seconds > 0 {
				if seconds > 60 {
					seconds = 60
				}
				return time.Duration(seconds) * time.Second
			}
		}
		if when, err := httpDate(value); err == nil {
			delay := when.Sub(now)
			if delay < time.Second {
				return time.Second
			}
			if delay > time.Minute {
				return time.Minute
			}
			return delay
		}
	}
	return 5 * time.Second
}

func httpDate(value string) (time.Time, error) {
	return time.Parse(time.RFC1123, strings.TrimSpace(value))
}
