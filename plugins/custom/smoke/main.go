// Command smoke loads packaged plugins through Navidrome's generated host
// functions and invokes their real WASM exports with deterministic host data.
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/navidrome/navidrome/plugins"
	"github.com/navidrome/navidrome/plugins/host"
)

const (
	albumMBID  = "12345678-1234-1234-1234-123456789abc"
	artistMBID = "abcdefab-cdef-abcd-efab-cdefabcdefab"
)

type albumRequest struct {
	Name   string `json:"name"`
	Artist string `json:"artist"`
	MBID   string `json:"mbid"`
}

type artistRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MBID string `json:"mbid"`
}

type imageInfo struct {
	URL  string `json:"url"`
	Size int32  `json:"size"`
}

type albumImagesResponse struct {
	Images []imageInfo `json:"images"`
}

type artistURLResponse struct {
	URL string `json:"url"`
}

type artistBiographyResponse struct {
	Biography string `json:"biography"`
}

type artistImagesResponse struct {
	Images []imageInfo `json:"images"`
}

type fakeHTTP struct {
	mu       sync.Mutex
	requests []host.HTTPRequest
}

func (h *fakeHTTP) Send(_ context.Context, request host.HTTPRequest) (*host.HTTPResponse, error) {
	if !request.NoFollowRedirects || request.TimeoutMs <= 0 || request.TimeoutMs > 2500 {
		return nil, fmt.Errorf("unsafe request flags: %#v", request)
	}
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	switch {
	case strings.HasPrefix(request.URL, "https://coverartarchive.org/release/"):
		return &host.HTTPResponse{StatusCode: 200, Headers: map[string]string{}, Body: []byte(`{"images":[{"front":true,"image":"https://archive.org/download/mock/front.jpg","thumbnails":{"250":"https://archive.org/download/mock/250.jpg","500":"https://archive.org/download/mock/500.jpg","1200":"https://archive.org/download/mock/1200.jpg"}}]}`)}, nil
	case request.URL == "https://query.wikidata.org/sparql":
		return &host.HTTPResponse{StatusCode: 200, Headers: map[string]string{}, Body: []byte(`{"results":{"bindings":[{"artist":{"value":"https://www.wikidata.org/entity/Q5383"},"label":{"value":"David Bowie"},"sitelink":{"value":"https://en.wikipedia.org/wiki/David_Bowie"},"image":{"value":"https://commons.wikimedia.org/wiki/Special:FilePath/David_Bowie_1974.jpg"}}]}}`)}, nil
	case strings.HasPrefix(request.URL, "https://en.wikipedia.org/w/api.php?"):
		return &host.HTTPResponse{StatusCode: 200, Headers: map[string]string{}, Body: []byte(`{"query":{"pages":{"1":{"extract":"David Bowie was an English singer and songwriter."}}}}`)}, nil
	default:
		return nil, fmt.Errorf("unexpected mock HTTP URL %s", request.URL)
	}
}

func (h *fakeHTTP) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

type fakeCache struct {
	mu      sync.Mutex
	strings map[string]string
	ints    map[string]int64
}

func newFakeCache() *fakeCache {
	return &fakeCache{strings: map[string]string{}, ints: map[string]int64{}}
}

func (c *fakeCache) SetString(_ context.Context, key, value string, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strings[key] = value
	return nil
}

func (c *fakeCache) GetString(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists := c.strings[key]
	return value, exists, nil
}

func (c *fakeCache) SetInt(_ context.Context, key string, value int64, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ints[key] = value
	return nil
}

func (c *fakeCache) GetInt(_ context.Context, key string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists := c.ints[key]
	return value, exists, nil
}

func (c *fakeCache) SetFloat(context.Context, string, float64, int64) error {
	return errors.New("unused")
}
func (c *fakeCache) GetFloat(context.Context, string) (float64, bool, error) {
	return 0, false, errors.New("unused")
}
func (c *fakeCache) SetBytes(context.Context, string, []byte, int64) error {
	return errors.New("unused")
}
func (c *fakeCache) GetBytes(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("unused")
}

func (c *fakeCache) Has(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, stringFound := c.strings[key]
	_, intFound := c.ints[key]
	return stringFound || intFound, nil
}

func (c *fakeCache) Remove(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.strings, key)
	delete(c.ints, key)
	return nil
}

type fakeConfig struct{}

func (fakeConfig) Get(_ context.Context, key string) (string, bool) {
	if key == "wikipediaLanguage" {
		return "en", true
	}
	return "", false
}
func (fakeConfig) GetInt(context.Context, string) (int64, bool) { return 0, false }
func (fakeConfig) Keys(context.Context, string) []string        { return []string{"wikipediaLanguage"} }

type runner struct {
	ctx      context.Context
	compiled *extism.CompiledPlugin
}

func newRunner(path string, http host.HTTPService, cache host.CacheService) (*runner, *plugins.Manifest, error) {
	manifest, err := plugins.ReadManifest(path)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest validation for %s: %w", path, err)
	}
	wasm, err := readPluginWasm(path)
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	wasmManifest := extism.Manifest{Wasm: []extism.Wasm{extism.WasmData{Data: wasm, Name: "plugin.wasm"}}}
	hostFunctions := append(host.RegisterHTTPHostFunctions(http), host.RegisterCacheHostFunctions(cache)...)
	hostFunctions = append(hostFunctions, host.RegisterConfigHostFunctions(fakeConfig{})...)
	compiled, err := extism.NewCompiledPlugin(ctx, wasmManifest, extism.PluginConfig{EnableWasi: true}, hostFunctions)
	if err != nil {
		return nil, nil, fmt.Errorf("compile WASM module %s: %w", path, err)
	}
	return &runner{ctx: ctx, compiled: compiled}, manifest, nil
}

func (r *runner) close() { _ = r.compiled.Close(r.ctx) }

func (r *runner) call(function string, input any, output any) error {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return err
	}
	instance, err := r.compiled.Instance(r.ctx, extism.PluginInstanceConfig{})
	if err != nil {
		return err
	}
	defer instance.Close(r.ctx)
	exitCode, data, err := instance.Call(function, inputJSON)
	if err != nil {
		return fmt.Errorf("WASM export %s: %w", function, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("WASM export %s returned exit code %d: %s", function, exitCode, string(data))
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s output %q: %w", function, string(data), err)
	}
	return nil
}

func readPluginWasm(path string) ([]byte, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	var names []string
	var wasm []byte
	for _, entry := range archive.File {
		names = append(names, entry.Name)
		if entry.Name != "plugin.wasm" {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, err
		}
		wasm, err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "manifest.json" || names[1] != "plugin.wasm" || len(wasm) == 0 {
		return nil, fmt.Errorf("package must contain exactly manifest.json and plugin.wasm; found %v", names)
	}
	return wasm, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./plugins/custom/smoke <coverartarchive.ndp> <wikimedia.ndp>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "WASM smoke failed:", err)
		os.Exit(1)
	}
}

func run(caaPath, wikimediaPath string) error {
	ctx := context.Background()
	caaHTTP, caaCache := &fakeHTTP{}, newFakeCache()
	caa, caaManifest, err := newRunner(caaPath, caaHTTP, caaCache)
	if err != nil {
		return err
	}
	defer caa.close()
	if caaManifest.Name != "coverartarchive" {
		return fmt.Errorf("unexpected CAA manifest name %q", caaManifest.Name)
	}
	album := albumRequest{MBID: albumMBID, Name: "Mock Album", Artist: "Mock Artist"}
	var albumResult albumImagesResponse
	var durations []time.Duration
	for i := 0; i < 100; i++ {
		started := time.Now()
		if err := caa.call("nd_get_album_images", album, &albumResult); err != nil {
			return err
		}
		durations = append(durations, time.Since(started))
		if len(albumResult.Images) != 4 {
			return fmt.Errorf("CAA export returned %d images, want 4", len(albumResult.Images))
		}
	}
	if got := caaHTTP.count(); got != 1 {
		return fmt.Errorf("CAA host-cache warm calls issued %d HTTP requests, want 1", got)
	}
	sort.Slice(durations[1:], func(i, j int) bool { return durations[1+i] < durations[1+j] })
	fmt.Printf("coverartarchive: export=nd_get_album_images calls=100 mock_http_calls=%d cold=%s warm_median=%s warm_p95=%s\n", caaHTTP.count(), durations[0], durations[len(durations)/2], durations[95])

	wikimediaHTTP, wikimediaCache := &fakeHTTP{}, newFakeCache()
	wikimedia, wikimediaManifest, err := newRunner(wikimediaPath, wikimediaHTTP, wikimediaCache)
	if err != nil {
		return err
	}
	defer wikimedia.close()
	if wikimediaManifest.Name != "wikimedia" {
		return fmt.Errorf("unexpected Wikimedia manifest name %q", wikimediaManifest.Name)
	}
	artist := artistRequest{ID: "artist-1", MBID: artistMBID, Name: "David Bowie"}
	var artistURL artistURLResponse
	if err := wikimedia.call("nd_get_artist_url", artist, &artistURL); err != nil {
		return err
	}
	var biography artistBiographyResponse
	if err := wikimedia.call("nd_get_artist_biography", artist, &biography); err != nil {
		return err
	}
	var artistImages artistImagesResponse
	if err := wikimedia.call("nd_get_artist_images", artist, &artistImages); err != nil {
		return err
	}
	if artistURL.URL != "https://en.wikipedia.org/wiki/David_Bowie" || biography.Biography == "" || len(artistImages.Images) != 1 {
		return fmt.Errorf("Wikimedia exports returned incomplete metadata: url=%q biography=%q images=%d", artistURL.URL, biography.Biography, len(artistImages.Images))
	}
	if got := wikimediaHTTP.count(); got != 2 {
		return fmt.Errorf("Wikimedia URL/bio/image exports made %d requests, want shared Wikidata + Wikipedia requests (2)", got)
	}
	fmt.Printf("wikimedia: exports=nd_get_artist_url,nd_get_artist_biography,nd_get_artist_images mock_http_calls=%d cache_shared=true\n", wikimediaHTTP.count())
	_ = ctx
	return nil
}
