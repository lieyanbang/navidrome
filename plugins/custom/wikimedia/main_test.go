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

const testArtistMBID = "abcdefab-cdef-abcd-efab-cdefabcdefab"

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

func newTestClient(http shared.HTTPDoer, cache shared.CacheBackend, language string) *pluginClient {
	return newClient(http, cache, language, time.Now, func(time.Duration) {})
}

func TestWikidataURLBiographyAndImageShareCachedArtistLookup(t *testing.T) {
	var sparqlCalls, wikipediaCalls int
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		switch {
		case request.URL == wikidataEndpoint:
			sparqlCalls++
			if request.Method != "POST" || request.Headers["User-Agent"] != wikipediaUA {
				t.Fatalf("unexpected SPARQL request: %#v", request)
			}
			return response(200, `{"results":{"bindings":[{"artist":{"value":"http://www.wikidata.org/entity/Q42"},"label":{"value":"David Bowie"},"sitelink":{"value":"https://en.wikipedia.org/wiki/David_Bowie"},"image":{"value":"https://commons.wikimedia.org/wiki/Special:FilePath/David_Bowie_1974.jpg"}}]}}`), nil
		case strings.Contains(request.URL, "en.wikipedia.org/w/api.php"):
			wikipediaCalls++
			return response(200, `{"query":{"pages":{"1":{"extract":"David Bowie was an English singer and songwriter."}}}}`), nil
		default:
			return nil, fmt.Errorf("unexpected host request %q", request.URL)
		}
	}}
	client := newTestClient(http, newMemoryCache(), "en")
	request := metadata.ArtistRequest{MBID: testArtistMBID, Name: "David Bowie"}
	artist, found, err := client.wikidataArtist(request)
	if err != nil || !found {
		t.Fatalf("expected verified Wikidata artist, found=%v err=%v", found, err)
	}
	if artist.WikipediaURL != "https://en.wikipedia.org/wiki/David_Bowie" || artist.ImageURL == "" {
		t.Fatalf("missing verified URL or image: %#v", artist)
	}
	biography, found, err := client.wikipediaBiography(artist.WikipediaURL)
	if err != nil || !found || biography == "" {
		t.Fatalf("expected Wikipedia biography, found=%v biography=%q err=%v", found, biography, err)
	}
	if _, found, err = client.wikidataArtist(request); err != nil || !found {
		t.Fatalf("expected cached artist result, found=%v err=%v", found, err)
	}
	if sparqlCalls != 1 || wikipediaCalls != 1 {
		t.Fatalf("URL/bio/image flow should share provider cache, SPARQL=%d Wikipedia=%d", sparqlCalls, wikipediaCalls)
	}
}

func TestWikidataNameSearchEscapesUnicodeAndRejectsUnsafeInput(t *testing.T) {
	name := `小明 "测试" \\ 城市`
	var capturedQuery string
	http := &mockHTTP{handle: func(request host.HTTPRequest) (*host.HTTPResponse, error) {
		values, err := url.ParseQuery(string(request.Body))
		if err != nil {
			t.Fatal(err)
		}
		capturedQuery = values.Get("query")
		return response(200, `{"results":{"bindings":[{"artist":{"value":"https://www.wikidata.org/entity/Q123"},"label":{"value":"小明 \"测试\" \\\\ 城市"},"sitelink":{"value":"https://zh.wikipedia.org/wiki/小明"},"image":{"value":"http://commons.wikimedia.org/wiki/Special:FilePath/小明.jpg"}}]}}`), nil
	}}
	client := newTestClient(http, newMemoryCache(), "zh")
	data, found, err := client.wikidataArtist(metadata.ArtistRequest{Name: name, MBID: "malicious-not-a-uuid"})
	if err != nil || !found || data.WikipediaURL != "https://zh.wikipedia.org/wiki/%E5%B0%8F%E6%98%8E" || !strings.HasPrefix(data.ImageURL, "https://") {
		t.Fatalf("expected validated Unicode result, data=%#v found=%v err=%v", data, found, err)
	}
	if !strings.Contains(capturedQuery, `\"测试\"`) || !strings.Contains(capturedQuery, `\\\\`) || !strings.Contains(capturedQuery, "小明") || strings.Contains(capturedQuery, "malicious-not-a-uuid") {
		t.Fatalf("SPARQL literal was not safely escaped or invalid MBID was used: %q", capturedQuery)
	}
	before := len(http.snapshot())
	if _, found, err := client.wikidataArtist(metadata.ArtistRequest{Name: "Unsafe\nName", MBID: "bad"}); err != nil || found {
		t.Fatalf("control character should not be queried, found=%v err=%v", found, err)
	}
	if len(http.snapshot()) != before {
		t.Fatal("unsafe control character caused a provider request")
	}
}

func TestDBpediaFallbackRequiresExactUnambiguousArtistAndSafeURLs(t *testing.T) {
	valid := `{"results":{"bindings":[{"artist":{"value":"https://dbpedia.org/resource/Ada_Lovelace"},"name":{"value":"Ada Lovelace"},"wiki":{"value":"https://en.wikipedia.org/wiki/Ada_Lovelace"},"comment":{"value":"Ada Lovelace was an English mathematician."}}]}}`
	for name, tc := range map[string]struct {
		payload string
		want    bool
	}{
		"one exact result":  {payload: valid, want: true},
		"ambiguous results": {payload: strings.Replace(valid, `}]}}`, `},{"artist":{"value":"https://dbpedia.org/resource/Ada_Second"},"name":{"value":"Ada Lovelace"},"wiki":{"value":"https://en.wikipedia.org/wiki/Ada_Lovelace"}}]}}`, 1), want: false},
		"wrong entity host": {payload: strings.Replace(valid, "https://dbpedia.org/resource/Ada_Lovelace", "https://evil.example/resource/Ada_Lovelace", 1), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) { return response(200, tc.payload), nil }}
			client := newTestClient(http, newMemoryCache(), "en")
			result, found, err := client.dbpediaArtist("Ada Lovelace")
			if err != nil || found != tc.want {
				t.Fatalf("unexpected fallback result: %#v found=%v err=%v", result, found, err)
			}
			if found && (result.WikipediaURL == "" || result.Biography == "") {
				t.Fatalf("verified fallback omitted metadata: %#v", result)
			}
		})
	}
	client := newTestClient(nil, newMemoryCache(), "zh")
	if _, found, err := client.dbpediaArtist("Ada Lovelace"); err != nil || found {
		t.Fatalf("DBpedia should be disabled for non-English mode, found=%v err=%v", found, err)
	}
}

func TestWikimediaMissingMalformedRateLimitedAndTimedOutResponses(t *testing.T) {
	cases := []struct {
		name     string
		handler  func(host.HTTPRequest) (*host.HTTPResponse, error)
		wantCall int
	}{
		{name: "missing fields", handler: func(host.HTTPRequest) (*host.HTTPResponse, error) {
			return response(200, `{"results":{"bindings":[null]}}`), nil
		}, wantCall: 1},
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
			client := newTestClient(http, newMemoryCache(), "en")
			_, found, err := client.wikidataArtist(metadata.ArtistRequest{MBID: testArtistMBID, Name: "Test Artist"})
			if tc.name == "missing fields" {
				if err != nil || found {
					t.Fatalf("missing data should return an empty result, found=%v err=%v", found, err)
				}
			} else if err == nil {
				t.Fatal("expected transient provider error")
			}
			if calls != tc.wantCall {
				t.Fatalf("expected %d bounded HTTP calls, got %d", tc.wantCall, calls)
			}
		})
	}
}

func TestWikidataThrottleSpacesDistinctRequests(t *testing.T) {
	var now = time.Unix(100, 0)
	var slept time.Duration
	http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) {
		return response(200, `{"results":{"bindings":[]}}`), nil
	}}
	client := newClient(http, newMemoryCache(), "en", func() time.Time { return now }, func(wait time.Duration) {
		slept += wait
		now = now.Add(wait)
	})
	for _, name := range []string{"Artist One", "Artist Two"} {
		if _, _, err := client.wikidataArtist(metadata.ArtistRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if slept != time.Second {
		t.Fatalf("expected one second provider pacing, got %s", slept)
	}
}

func TestInputAndReturnedURLValidation(t *testing.T) {
	if _, err := sparqlLiteral("bad\ninput"); err == nil {
		t.Fatal("SPARQL literal accepted a control character")
	}
	literal, err := sparqlLiteral(`A "quote" and \\ slash`)
	if err != nil || !strings.Contains(literal, `\"quote\"`) || !strings.Contains(literal, `\\\\`) {
		t.Fatalf("SPARQL literal did not escape quotes and backslashes: %q err=%v", literal, err)
	}
	if safeLanguage("xx") != "en" {
		t.Fatal("unsupported language should fall back to English")
	}
	if validWikidataEntityURL("https://www.wikidata.org/entity/Q0") || validWikidataEntityURL("https://evil.example/entity/Q42") {
		t.Fatal("accepted invalid Wikidata entity URL")
	}
	if validWikipediaPageURL("https://en.wikipedia.org.evil.test/wiki/Ada", "en") != "" || validWikipediaPageURL("http://en.wikipedia.org/wiki/Ada", "en") != "" {
		t.Fatal("accepted invalid Wikipedia URL")
	}
	if validWikimediaImageURL("https://evil.example/image.jpg") != "" || !strings.HasPrefix(validWikimediaImageURL("http://commons.wikimedia.org/wiki/Special:FilePath/Ada.jpg"), "https://") {
		t.Fatal("Wikimedia image host validation failed")
	}
	if !validDBpediaEntityURL("https://dbpedia.org/resource/Ada_Lovelace") || validDBpediaEntityURL("https://dbpedia.org.evil.test/resource/Ada") {
		t.Fatal("DBpedia entity URL validation failed")
	}
}

func TestConcurrentArtistLookupsStayCacheSafe(t *testing.T) {
	http := &mockHTTP{handle: func(host.HTTPRequest) (*host.HTTPResponse, error) {
		return response(200, `{"results":{"bindings":[{"artist":{"value":"https://www.wikidata.org/entity/Q123"},"label":{"value":"Shared Artist"},"sitelink":{"value":"https://en.wikipedia.org/wiki/Shared_Artist"}}]}}`), nil
	}}
	client := newTestClient(http, newMemoryCache(), "en")
	const workers = 24
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, found, err := client.wikidataArtist(metadata.ArtistRequest{Name: "Shared Artist"})
			if err != nil || !found {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("found=%v err=%v", found, err)
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
	if _, found, err := client.wikidataArtist(metadata.ArtistRequest{Name: "Shared Artist"}); err != nil || !found {
		t.Fatalf("expected warm cache result, found=%v err=%v", found, err)
	}
	if got := len(http.snapshot()); got != before {
		t.Fatalf("expected cached request, count changed from %d to %d", before, got)
	}
}
