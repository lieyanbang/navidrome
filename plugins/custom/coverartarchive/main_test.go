package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lieyanbang/navidrome/plugins/custom/shared"
	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
)

const testMBID = "12345678-1234-1234-1234-123456789abc"

type memoryCache struct {
	mu   sync.Mutex
	text map[string]string
	ints map[string]int64
}

func newMemoryCache() *memoryCache {
	return &memoryCache{text: map[string]string{}, ints: map[string]int64{}}
}

func (c *memoryCache) GetString(key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.text[key]
	return value, ok, nil
}

func (c *memoryCache) SetString(key, value string, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text[key] = value
	return nil
}

func (c *memoryCache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.text, key)
	return nil
}

func (c *memoryCache) GetInt(key string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.ints[key]
	return value, ok, nil
}

func (c *memoryCache) SetInt(key string, value, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ints[key] = value
	return nil
}

type mockHTTP struct {
	mu       sync.Mutex
	requests []host.HTTPRequest
	handle   func(host.HTTPRequest) (*host.HTTPResponse, error)
}

func (h *mockHTTP) Send(request host.HTTPRequest) (*host.HTTPResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	return h.handle(request)
}

func (h *mockHTTP) snapshot() []host.HTTPRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]host.HTTPRequest(nil), h.requests...)
}

func response(status int32, body string) *host.HTTPResponse {
	return &host.HTTPResponse{StatusCode: status, Body: []byte(body), Headers: map[string]string{}}
}

func newTestClient(http shared.HTTPDoer, cache shared.CacheBackend) *pluginClient {
	return newClient(http, cache, time.Now, func(time.Duration) {})
}

func TestGetAlbumInfoReturnsKnownReleaseWithoutNetwork(t *testing.T) {
	http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) {
		t.Fatal("known MBID lookup should not issue a metadata request")
		return nil, nil
	}}
	client := newTestClient(http, newMemoryCache())
	result, err := client.getAlbumInfo(metadata.AlbumRequest{MBID: strings.ToUpper(testMBID), Name: "Album"})
	if err != nil || result == nil {
		t.Fatalf("expected album info for known release, result=%#v err=%v", result, err)
	}
	if result.MBID != testMBID || result.Name != "Album" || result.URL != "https://musicbrainz.org/release/"+testMBID {
		t.Fatalf("unexpected album info: %#v", result)
	}
	if got := len(http.snapshot()); got != 0 {
		t.Fatalf("known MBID should be network-free, got %d requests", got)
	}
}

func TestGetAlbumInfoUsesExactReleaseGroupMatch(t *testing.T) {
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		if !strings.Contains(request.URL, "musicbrainz.org") {
			t.Fatalf("unexpected provider request %q", request.URL)
		}
		return response(200, `{"release-groups":[{"id":"12345678-1234-1234-1234-123456789abc","title":"Ferrari","artist-credit-phrase":"Bebe Rexha"}]}`), nil
	}}
	client := newTestClient(http, newMemoryCache())
	result, err := client.getAlbumInfo(metadata.AlbumRequest{Name: "Ferrari", Artist: "Bebe Rexha"})
	if err != nil || result == nil {
		t.Fatalf("expected exact release-group info, result=%#v err=%v", result, err)
	}
	if result.MBID != testMBID || result.URL != "https://musicbrainz.org/release-group/"+testMBID {
		t.Fatalf("unexpected release-group info: %#v", result)
	}
}

func TestReleaseMBIDFollowsOnlyExactCAAAndArchiveRedirects(t *testing.T) {
	archiveIndex := "https://archive.org/download/mbid-" + testMBID + "/index.json"
	cdnIndex := "https://dn711103.ca.archive.org/0/items/mbid-" + testMBID + "/index.json"
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		switch request.URL {
		case "https://coverartarchive.org/release/" + testMBID + "/":
			return &host.HTTPResponse{StatusCode: 307, Headers: map[string]string{"Location": archiveIndex}}, nil
		case archiveIndex:
			return &host.HTTPResponse{StatusCode: 302, Headers: map[string]string{"Location": cdnIndex}}, nil
		case cdnIndex:
			return response(200, `{"images":[{"front":true,"image":"https://archive.org/download/caa/front.jpg"}]}`), nil
		default:
			t.Fatalf("unexpected request %q", request.URL)
			return nil, nil
		}
	}}
	client := newTestClient(http, newMemoryCache())
	result, err := client.getAlbumImages(metadata.AlbumRequest{MBID: testMBID})
	if err != nil || result == nil || len(result.Images) != 1 {
		t.Fatalf("expected release artwork, result=%#v err=%v", result, err)
	}
	requests := http.snapshot()
	if len(requests) != 3 {
		t.Fatalf("expected three explicitly validated requests, got %#v", requests)
	}
	for _, request := range requests {
		if !request.NoFollowRedirects {
			t.Fatalf("implicit redirects must remain disabled: %#v", request)
		}
	}
}

func TestReleaseGroupRedirectCanResolveToItsMemberRelease(t *testing.T) {
	groupID := testMBID
	releaseID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	archiveIndex := "https://archive.org/download/mbid-" + releaseID + "/index.json"
	cdnIndex := "https://dn711103.ca.archive.org/0/items/mbid-" + releaseID + "/index.json"
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		switch request.URL {
		case "https://coverartarchive.org/release-group/" + groupID + "/":
			return &host.HTTPResponse{StatusCode: 307, Headers: map[string]string{"Location": archiveIndex}}, nil
		case archiveIndex:
			return &host.HTTPResponse{StatusCode: 302, Headers: map[string]string{"Location": cdnIndex}}, nil
		case cdnIndex:
			return response(200, `{"images":[{"front":true,"image":"https://coverartarchive.org/release/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/1.jpg"}]}`), nil
		default:
			t.Fatalf("unexpected request %q", request.URL)
			return nil, nil
		}
	}}
	client := newTestClient(http, newMemoryCache())
	images, found, err := client.caaImages("release-group", groupID)
	if err != nil || !found || len(images) != 1 {
		t.Fatalf("expected group art from its member release, images=%#v found=%v err=%v", images, found, err)
	}
	if got := len(http.snapshot()); got != 3 {
		t.Fatalf("expected CAA, archive index, and CDN requests, got %d", got)
	}
}

func TestArchiveMetadataMirrorsRecoverIndexAfter503(t *testing.T) {
	releaseID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	groupID := testMBID
	archiveIndex := "https://archive.org/download/mbid-" + releaseID + "/index.json"
	metadataURL := "https://archive.org/metadata/mbid-" + releaseID
	mirrorIndex := "https://ia600801.us.archive.org/2/items/mbid-" + releaseID + "/index.json"
	var archiveIndexCalls int
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		switch request.URL {
		case "https://coverartarchive.org/release-group/" + groupID + "/":
			return &host.HTTPResponse{StatusCode: 307, Headers: map[string]string{"Location": archiveIndex}}, nil
		case archiveIndex:
			archiveIndexCalls++
			return response(503, `{}`), nil
		case metadataURL:
			return response(200, `{"d1":"ia600801.us.archive.org","d2":"ia800801.us.archive.org","server":"ia800801.us.archive.org","dir":"/2/items/mbid-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","files":[{"name":"index.json"}]}`), nil
		case mirrorIndex:
			return response(200, `{"images":[{"front":true,"image":"https://coverartarchive.org/release/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/1.jpg","thumbnails":{"500":"https://coverartarchive.org/release/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/1-500.jpg"}}]}`), nil
		default:
			t.Fatalf("unexpected request %q", request.URL)
			return nil, nil
		}
	}}
	client := newTestClient(http, newMemoryCache())
	images, found, err := client.caaImages("release-group", groupID)
	if err != nil || !found || len(images) != 2 {
		t.Fatalf("expected artwork from Internet Archive mirror, images=%#v found=%v err=%v", images, found, err)
	}
	if archiveIndexCalls != 2 {
		t.Fatalf("expected one bounded retry for 503, got %d calls", archiveIndexCalls)
	}
	requests := http.snapshot()
	if len(requests) != 5 || requests[3].URL != metadataURL || requests[4].URL != mirrorIndex {
		t.Fatalf("unexpected mirror fallback requests: %#v", requests)
	}
	for _, request := range requests {
		if !request.NoFollowRedirects {
			t.Fatalf("implicit redirects must remain disabled: %#v", request)
		}
	}
}

func TestArchiveMetadataFallbackRejectsUntrustedHostsAndPaths(t *testing.T) {
	cases := map[string]string{
		"untrusted hosts": `{"d1":"archive.org.evil.test","d2":"attacker.example","server":"attacker.example","dir":"/2/items/mbid-` + testMBID + `","files":[{"name":"index.json"}]}`,
		"wrong item path": `{"d1":"ia600801.us.archive.org","d2":"ia800801.us.archive.org","dir":"/2/items/mbid-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","files":[{"name":"index.json"}]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
				if request.URL != "https://archive.org/metadata/mbid-"+testMBID {
					t.Fatalf("unexpected mirror request for invalid metadata: %q", request.URL)
				}
				return response(200, payload), nil
			}}
			client := newTestClient(http, newMemoryCache())
			result, err := client.archiveMetadataIndex(testMBID)
			if err == nil || result != nil {
				t.Fatalf("invalid archive metadata should fail closed, result=%#v err=%v", result, err)
			}
			if got := len(http.snapshot()); got != 1 {
				t.Fatalf("invalid mirror data caused an extra request, calls=%d", got)
			}
		})
	}
}

func TestReleaseMBIDRejectsUnexpectedRedirectTargets(t *testing.T) {
	archiveIndex := "https://archive.org/download/mbid-" + testMBID + "/index.json"
	for name, firstTarget := range map[string]string{
		"wrong scheme":    strings.Replace(archiveIndex, "https://", "http://", 1),
		"wrong host":      "https://archive.org.evil.test/download/mbid-" + testMBID + "/index.json",
		"wrong MBID path": "https://archive.org/download/mbid-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/index.json",
	} {
		t.Run(name, func(t *testing.T) {
			http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
				return &host.HTTPResponse{StatusCode: 307, Headers: map[string]string{"Location": firstTarget}}, nil
			}}
			client := newTestClient(http, newMemoryCache())
			result, err := client.getAlbumImages(metadata.AlbumRequest{MBID: testMBID})
			if err == nil || result != nil {
				t.Fatalf("unexpected redirect target should fail closed, result=%#v err=%v", result, err)
			}
			if got := len(http.snapshot()); got != 1 {
				t.Fatalf("untrusted target was requested, calls=%d", got)
			}
		})
	}
	for name, cdnTarget := range map[string]string{
		"wrong CDN host":  "https://archive.org.evil.test/0/items/mbid-" + testMBID + "/index.json",
		"wrong CDN path":  "https://dn711103.ca.archive.org/download/mbid-" + testMBID + "/index.json",
		"wrong CDN query": "https://dn711103.ca.archive.org/0/items/mbid-" + testMBID + "/index.json?x=1",
	} {
		t.Run(name, func(t *testing.T) {
			http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
				if request.URL == "https://coverartarchive.org/release/"+testMBID+"/" {
					return &host.HTTPResponse{StatusCode: 307, Headers: map[string]string{"Location": archiveIndex}}, nil
				}
				return &host.HTTPResponse{StatusCode: 302, Headers: map[string]string{"Location": cdnTarget}}, nil
			}}
			client := newTestClient(http, newMemoryCache())
			result, err := client.getAlbumImages(metadata.AlbumRequest{MBID: testMBID})
			if err == nil || result != nil {
				t.Fatalf("unexpected CDN redirect should fail closed, result=%#v err=%v", result, err)
			}
			if got := len(http.snapshot()); got != 2 {
				t.Fatalf("untrusted CDN target was requested, calls=%d", got)
			}
		})
	}
}

func TestKnownMBIDPrefersFrontAndWarmsCacheFor100Calls(t *testing.T) {
	http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) {
		return response(200, `{"images":[{"front":false,"image":"https://archive.org/download/caa/back.jpg"},{"front":true,"image":"https://archive.org/download/caa/front.jpg","thumbnails":{"250":"https://archive.org/download/caa/250.jpg","500":"https://archive.org/download/caa/500.jpg","1200":"https://archive.org/download/caa/1200.jpg"}}]}`), nil
	}}
	client := newTestClient(http, newMemoryCache())
	request := metadata.AlbumRequest{MBID: testMBID, Name: "Album", Artist: "Artist"}
	for i := 0; i < 100; i++ {
		result, err := client.getAlbumImages(request)
		if err != nil || result == nil || len(result.Images) != 4 {
			t.Fatalf("call %d: expected four front-cover sizes, result=%#v err=%v", i, result, err)
		}
		if result.Images[0].URL != "https://archive.org/download/caa/250.jpg" || result.Images[3].URL != "https://archive.org/download/caa/front.jpg" {
			t.Fatalf("unexpected image list: %#v", result.Images)
		}
	}
	if got := len(http.snapshot()); got != 1 {
		t.Fatalf("100 calls should use one provider request after cache warmup, got %d", got)
	}
}

func TestNameFallbackEscapesLuceneAndRequiresExactTitleAndArtist(t *testing.T) {
	name := `Beyoncé "Live" \\ 東京`
	artist := "嵐"
	var imageCalls int
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		if strings.Contains(request.URL, "musicbrainz.org") {
			parsed, err := url.Parse(request.URL)
			if err != nil {
				t.Fatal(err)
			}
			query := parsed.Query().Get("query")
			if !strings.Contains(query, `Beyoncé \"Live\"`) || !strings.Contains(query, `\\\\`) || !strings.Contains(query, "東京") {
				t.Fatalf("Lucene phrase was not escaped safely: %q", query)
			}
			return response(200, fmt.Sprintf(`{"release-groups":[{"id":%q,"title":%q,"artist-credit":[{"name":%q,"artist":{"name":%q}}]}]}`, testMBID, name, artist, artist)), nil
		}
		imageCalls++
		return response(200, `{"images":[{"front":true,"image":"https://archive.org/download/caa/front.jpg"}]}`), nil
	}}
	client := newTestClient(http, newMemoryCache())
	result, err := client.getAlbumImages(metadata.AlbumRequest{Name: name, Artist: artist})
	if err != nil || result == nil || len(result.Images) != 1 {
		t.Fatalf("expected exact Unicode match, result=%#v err=%v", result, err)
	}
	if imageCalls != 1 {
		t.Fatalf("expected one CAA lookup, got %d", imageCalls)
	}
}

func TestSearchRejectsWrongArtistAndAmbiguousExactCandidates(t *testing.T) {
	wrongArtist := `{"release-groups":[{"id":"12345678-1234-1234-1234-123456789abc","title":"Same","artist-credit-phrase":"Somebody Else"}]}`
	ambiguous := `{"release-groups":[{"id":"12345678-1234-1234-1234-123456789abc","title":"Same","artist-credit-phrase":"Artist"},{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","title":"Same","artist-credit-phrase":"Artist"}]}`
	for name, payload := range map[string]string{"wrong artist": wrongArtist, "ambiguous": ambiguous} {
		t.Run(name, func(t *testing.T) {
			http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) { return response(200, payload), nil }}
			client := newTestClient(http, newMemoryCache())
			result, err := client.getAlbumImages(metadata.AlbumRequest{Name: "Same", Artist: "Artist"})
			if err != nil || result != nil {
				t.Fatalf("unsafe candidate accepted: result=%#v err=%v", result, err)
			}
			if got := len(http.snapshot()); got != 1 {
				t.Fatalf("expected search only, got %d provider calls", got)
			}
		})
	}
}

func TestUnknownMBIDAndNoArtUseNegativeCache(t *testing.T) {
	var calls int
	http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) {
		calls++
		return response(404, `{}`), nil
	}}
	client := newTestClient(http, newMemoryCache())
	for i := 0; i < 2; i++ {
		result, err := client.getAlbumImages(metadata.AlbumRequest{MBID: testMBID})
		if err != nil || result != nil {
			t.Fatalf("expected no art without an error, result=%#v err=%v", result, err)
		}
	}
	if calls != 2 { // release and release-group are each queried once; repeat is cached.
		t.Fatalf("expected two cached negative lookups, got %d", calls)
	}
	if _, _, err := client.caaImages("release", "bad-mbid"); err != nil {
		t.Fatalf("invalid MBID should not issue a request: %v", err)
	}
	if calls != 2 {
		t.Fatalf("invalid MBID issued an unexpected request, calls=%d", calls)
	}
}

func TestMalformedOversizedRateLimitServerErrorAndTimeoutAreRetryable(t *testing.T) {
	cases := []struct {
		name     string
		handler  func(host.HTTPRequest) (*host.HTTPResponse, error)
		wantCall int
	}{
		{name: "malformed JSON", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) { return response(200, `{`), nil }, wantCall: 1},
		{name: "oversized JSON", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) {
			return &host.HTTPResponse{StatusCode: 200, Body: make([]byte, shared.DefaultResponseLimit+1)}, nil
		}, wantCall: 1},
		{name: "429", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) { return response(429, `{}`), nil }, wantCall: 1},
		{name: "500", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) { return response(500, `{}`), nil }, wantCall: 1},
		{name: "503", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) { return response(503, `{}`), nil }, wantCall: 2},
		{name: "timeout", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) { return nil, errors.New("timeout") }, wantCall: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
				calls++
				return tc.handler(request)
			}}
			client := newTestClient(http, newMemoryCache())
			result, err := client.getAlbumImages(metadata.AlbumRequest{MBID: testMBID})
			if err == nil || result != nil {
				t.Fatalf("expected retryable provider error, result=%#v err=%v", result, err)
			}
			if calls != tc.wantCall {
				t.Fatalf("expected %d bounded HTTP calls, got %d", tc.wantCall, calls)
			}
		})
	}
}

func TestImageURLsAreRestrictedToKnownHTTPSHostsAndPaths(t *testing.T) {
	for _, raw := range []string{
		"http://archive.org/download/a/front.jpg",
		"https://evilarchive.org/download/a/front.jpg",
		"https://archive.org.evil.test/download/a/front.jpg",
		"https://archive.org:8443/download/a/front.jpg",
		"https://user@archive.org/download/a/front.jpg",
		"https://archive.org/other/front.jpg",
	} {
		if got := validCAAImageURL(raw); got != "" {
			t.Errorf("accepted unsafe URL %q as %q", raw, got)
		}
	}
	if got := validCAAImageURL("https://ia800.us.archive.org/download/a/front.jpg"); got == "" {
		t.Fatal("rejected a valid archive.org CDN URL")
	}
}

func TestConcurrentKnownMBIDCallsRemainSafeAndCacheResult(t *testing.T) {
	http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) {
		return response(200, `{"images":[{"front":true,"image":"https://archive.org/download/caa/front.jpg"}]}`), nil
	}}
	backend := newMemoryCache()
	client := newTestClient(http, backend)
	request := metadata.AlbumRequest{MBID: testMBID}
	const workers = 32
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := client.getAlbumImages(request)
			if err != nil || result == nil || len(result.Images) != 1 {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("result=%#v err=%v", result, err)
				}
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	before := len(http.snapshot())
	if before < 1 || before > workers {
		t.Fatalf("unexpected concurrent request count %d", before)
	}
	if _, err := client.getAlbumImages(request); err != nil {
		t.Fatal(err)
	}
	if got := len(http.snapshot()); got != before {
		t.Fatalf("expected cached warm call, request count changed from %d to %d", before, got)
	}
}
