package shared

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
)

type testCache struct {
	mu      sync.Mutex
	strings map[string]string
	ints    map[string]int64
}

func newTestCache() *testCache {
	return &testCache{strings: map[string]string{}, ints: map[string]int64{}}
}

func (c *testCache) GetString(key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.strings[key]
	return value, ok, nil
}

func (c *testCache) SetString(key, value string, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strings[key] = value
	return nil
}

func (c *testCache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.strings, key)
	return nil
}

func (c *testCache) GetInt(key string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.ints[key]
	return value, ok, nil
}

func (c *testCache) SetInt(key string, value, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ints[key] = value
	return nil
}

type testHTTP struct {
	requests []host.HTTPRequest
	response *host.HTTPResponse
	err      error
}

func (h *testHTTP) Send(request host.HTTPRequest) (*host.HTTPResponse, error) {
	h.requests = append(h.requests, request)
	return h.response, h.err
}

func TestBoundedCacheEvictsOldestEntry(t *testing.T) {
	backend := newTestCache()
	cache := NewBoundedCache(backend, "test", 2, 1024)
	for _, key := range []string{"one", "two", "three"} {
		if err := cache.SetJSON(key, map[string]string{"key": key}, 60); err != nil {
			t.Fatal(err)
		}
	}

	var value map[string]string
	if cache.GetJSON("one", &value) {
		t.Fatal("oldest entry was not evicted")
	}
	if !cache.GetJSON("two", &value) || value["key"] != "two" {
		t.Fatal("expected second entry to remain")
	}
	if !cache.GetJSON("three", &value) || value["key"] != "three" {
		t.Fatal("expected newest entry to remain")
	}
}

func TestBoundedCacheRejectsOversizedValues(t *testing.T) {
	cache := NewBoundedCache(newTestCache(), "test", 2, 8)
	err := cache.SetJSON("too-large", strings.Repeat("x", 32), 60)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected bounded cache error, got %v", err)
	}
}

func TestBoundedCacheEvictsAtCapacityWithLongPrefix(t *testing.T) {
	backend := newTestCache()
	cache := NewBoundedCache(backend, "wikimedia", 64, 24*1024)
	for i := 0; i < 65; i++ {
		if err := cache.SetJSON(fmt.Sprintf("key-%02d", i), map[string]int{"index": i}, 60); err != nil {
			t.Fatal(err)
		}
	}
	var value map[string]int
	if cache.GetJSON("key-00", &value) {
		t.Fatal("oldest entry was not evicted at the configured capacity")
	}
	if !cache.GetJSON("key-64", &value) || value["index"] != 64 {
		t.Fatal("newest entry was not retained")
	}
}

func TestBoundedCacheNilBackendFailsClosed(t *testing.T) {
	cache := NewBoundedCache(nil, "test", 2, 128)
	var value map[string]string
	if cache.GetJSON("key", &value) {
		t.Fatal("nil cache backend unexpectedly returned a value")
	}
	if err := cache.SetJSON("key", map[string]string{"value": "x"}, 60); err == nil {
		t.Fatal("nil cache backend should report a write error")
	}
	if err := cache.Throttle("provider", time.Second, time.Now().Add(time.Second), time.Now, func(time.Duration) {}); err == nil {
		t.Fatal("nil cache backend should fail closed for provider pacing")
	}
}

func TestHTTPClientDisablesRedirectsAndBoundsResponse(t *testing.T) {
	now := time.Now()
	http := &testHTTP{response: &host.HTTPResponse{StatusCode: 302, Headers: map[string]string{"location": " https://example.org/target "}}}
	client := NewHTTPClient(http, now.Add(5*time.Second), func() time.Time { return now }, func(time.Duration) {})
	_, err := client.Send(host.HTTPRequest{Method: "GET", URL: "https://api.example.org/lookup"}, []string{"api.example.org"})
	var redirect *RedirectError
	if !errors.As(err, &redirect) {
		t.Fatalf("expected blocked redirect, got %v", err)
	}
	if redirect.Location != "https://example.org/target" {
		t.Fatalf("expected redirect target to be preserved for explicit validation, got %q", redirect.Location)
	}
	if len(http.requests) != 1 || !http.requests[0].NoFollowRedirects {
		t.Fatalf("unexpected redirect behavior: %#v", http.requests)
	}
	if http.requests[0].TimeoutMs <= 0 || http.requests[0].TimeoutMs > 2500 {
		t.Fatalf("unexpected request timeout: %d", http.requests[0].TimeoutMs)
	}

	http = &testHTTP{response: &host.HTTPResponse{StatusCode: 200, Body: make([]byte, DefaultResponseLimit+1)}}
	client = NewHTTPClient(http, now.Add(5*time.Second), func() time.Time { return now }, func(time.Duration) {})
	_, err = client.Send(host.HTTPRequest{Method: "GET", URL: "https://api.example.org/lookup"}, []string{"api.example.org"})
	var large *BodyTooLargeError
	if !errors.As(err, &large) {
		t.Fatalf("expected response size error, got %v", err)
	}
}

func TestValidateHTTPSURLRequiresExactAllowedHost(t *testing.T) {
	for _, raw := range []string{
		"http://api.example.org/path",
		"https://user@api.example.org/path",
		"https://api.example.org:8443/path",
		"https://api.example.org.evil.test/path",
		"https://api.example.org/path#fragment",
	} {
		if _, err := ValidateHTTPSURL(raw, []string{"api.example.org"}); err == nil {
			t.Errorf("expected %q to be rejected", raw)
		}
	}
	if _, err := ValidateHTTPSURL("https://API.EXAMPLE.ORG/path", []string{"api.example.org"}); err != nil {
		t.Fatalf("expected case-insensitive host match: %v", err)
	}
	if _, err := url.Parse("https://example.org"); err != nil {
		t.Fatal(err)
	}
}

func TestThrottleSpacesSequentialRequests(t *testing.T) {
	backend := newTestCache()
	cache := NewBoundedCache(backend, "test", 4, 1024)
	now := time.Unix(100, 0)
	slept := time.Duration(0)
	sleep := func(duration time.Duration) {
		slept += duration
		now = now.Add(duration)
	}
	deadline := now.Add(5 * time.Second)
	if err := cache.Throttle("musicbrainz", time.Second, deadline, func() time.Time { return now }, sleep); err != nil {
		t.Fatal(err)
	}
	now = now.Add(400 * time.Millisecond)
	if err := cache.Throttle("musicbrainz", time.Second, deadline, func() time.Time { return now }, sleep); err != nil {
		t.Fatal(err)
	}
	if slept != 600*time.Millisecond {
		t.Fatalf("expected 600ms spacing, got %s", slept)
	}
}

func TestHTTPClientRetriesOneGatewayFailureAndHonorsRateLimit(t *testing.T) {
	now := time.Unix(100, 0)
	responses := []*host.HTTPResponse{
		{StatusCode: 503, Headers: map[string]string{}},
		{StatusCode: 200, Body: []byte(`{"ok":true}`)},
	}
	// Replace the static fake with a small sequenced doer for this case.
	sequenced := &sequencedHTTP{responses: responses}
	client := NewHTTPClient(sequenced, now.Add(5*time.Second), func() time.Time { return now }, func(delay time.Duration) { now = now.Add(delay) })
	if _, err := client.Send(host.HTTPRequest{Method: "GET", URL: "https://api.example.org/lookup"}, []string{"api.example.org"}); err != nil {
		t.Fatal(err)
	}
	if sequenced.calls != 2 {
		t.Fatalf("expected one retry after 503, got %d calls", sequenced.calls)
	}

	limited := &testHTTP{response: &host.HTTPResponse{StatusCode: 429, Headers: map[string]string{"Retry-After": "7"}}}
	client = NewHTTPClient(limited, now.Add(5*time.Second), func() time.Time { return now }, func(time.Duration) {})
	_, err := client.Send(host.HTTPRequest{Method: "GET", URL: "https://api.example.org/lookup"}, []string{"api.example.org"})
	if got := RetryDelay(err); got != 7*time.Second {
		t.Fatalf("expected Retry-After to be honored, got %s (%v)", got, err)
	}

	zeroRetryAfter := &testHTTP{response: &host.HTTPResponse{StatusCode: 429, Headers: map[string]string{"Retry-After": "0"}}}
	client = NewHTTPClient(zeroRetryAfter, now.Add(10*time.Second), func() time.Time { return now }, func(time.Duration) {})
	_, err = client.Send(host.HTTPRequest{Method: "GET", URL: "https://api.example.org/lookup"}, []string{"api.example.org"})
	if got := RetryDelay(err); got != 5*time.Second {
		t.Fatalf("expected non-positive Retry-After to use the 5s default, got %s (%v)", got, err)
	}
}

func TestHTTPClientDoesNotRetryMusicBrainzOrFollowProviderRedirects(t *testing.T) {
	sequenced := &sequencedHTTP{responses: []*host.HTTPResponse{{StatusCode: 503, Headers: map[string]string{"Retry-After": "0"}}, {StatusCode: 200}}}
	client := NewHTTPClient(sequenced, time.Now().Add(5*time.Second), time.Now, func(time.Duration) {})
	_, err := client.Send(host.HTTPRequest{Method: "GET", URL: "https://musicbrainz.org/ws/2/release-group/"}, []string{"musicbrainz.org"})
	if err == nil || sequenced.calls != 1 {
		t.Fatalf("MusicBrainz 503 should not be retried, err=%v calls=%d", err, sequenced.calls)
	}
	if got := RetryDelay(err); got != 5*time.Second {
		t.Fatalf("expected MusicBrainz 503 Retry-After: 0 to fall back to 5s, got %s (%v)", got, err)
	}
}

type sequencedHTTP struct {
	responses []*host.HTTPResponse
	calls     int
}

func (h *sequencedHTTP) Send(host.HTTPRequest) (*host.HTTPResponse, error) {
	index := h.calls
	h.calls++
	if index >= len(h.responses) {
		return nil, errors.New("unexpected extra HTTP call")
	}
	return h.responses[index], nil
}
