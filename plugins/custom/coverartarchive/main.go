package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lieyanbang/navidrome/plugins/custom/shared"
	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
)

const (
	musicBrainzURL = "https://musicbrainz.org/ws/2/release-group/"
	caaURL         = "https://coverartarchive.org/"
	userAgent      = "NavidromeCoverArtArchive/1.1.1 (+https://github.com/lieyanbang/navidrome)"
	positiveTTL    = int64(30 * 24 * 60 * 60)
	negativeTTL    = int64(6 * 60 * 60)
	maxNameBytes   = 256
)

var mbidPattern = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

type caaPlugin struct{}

func init() {
	metadata.Register(&caaPlugin{})
}

var _ metadata.AlbumImagesProvider = (*caaPlugin)(nil)

func (*caaPlugin) GetAlbumImages(request metadata.AlbumRequest) (*metadata.AlbumImagesResponse, error) {
	return newClient(nil, shared.HostCache{}, time.Now, time.Sleep).getAlbumImages(request)
}

type pluginClient struct {
	http     *shared.HTTPClient
	cache    *shared.BoundedCache
	deadline time.Time
	now      func() time.Time
	sleep    func(time.Duration)
}

func newClient(doer shared.HTTPDoer, cache shared.CacheBackend, now func() time.Time, sleep func(time.Duration)) *pluginClient {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := now().Add(shared.DefaultTotalTimeout)
	return &pluginClient{
		http:     shared.NewHTTPClient(doer, deadline, now, sleep),
		cache:    shared.NewBoundedCache(cache, "caa", 64, 32*1024),
		deadline: deadline,
		now:      now,
		sleep:    sleep,
	}
}

func (c *pluginClient) getAlbumImages(request metadata.AlbumRequest) (*metadata.AlbumImagesResponse, error) {
	name := strings.TrimSpace(request.Name)
	artist := strings.TrimSpace(request.Artist)
	mbid := strings.ToLower(strings.TrimSpace(request.MBID))
	if len(name) > maxNameBytes || len(artist) > maxNameBytes {
		return nil, nil
	}

	if validMBID(mbid) {
		for _, kind := range []string{"release", "release-group"} {
			images, found, err := c.caaImages(kind, mbid)
			if err != nil {
				return nil, retryLater(err)
			}
			if found {
				return &metadata.AlbumImagesResponse{Images: images}, nil
			}
		}
	}

	if name == "" || artist == "" {
		return nil, nil
	}
	releaseGroupID, found, err := c.searchReleaseGroup(name, artist)
	if err != nil {
		return nil, retryLater(err)
	}
	if !found {
		return nil, nil
	}
	images, found, err := c.caaImages("release-group", releaseGroupID)
	if err != nil {
		return nil, retryLater(err)
	}
	if !found {
		return nil, nil
	}
	return &metadata.AlbumImagesResponse{Images: images}, nil
}

func validMBID(value string) bool {
	return mbidPattern.MatchString(value)
}

type cachedImages struct {
	Found  bool                 "json:\"found\""
	Images []metadata.ImageInfo "json:\"images,omitempty\""
}

func (c *pluginClient) caaImages(kind, id string) ([]metadata.ImageInfo, bool, error) {
	key := "caa:" + kind + ":" + strings.ToLower(id)
	var cached cachedImages
	if c.cache.GetJSON(key, &cached) {
		return cached.Images, cached.Found, nil
	}

	if !validMBID(id) || (kind != "release" && kind != "release-group") {
		return nil, false, nil
	}
	target := caaURL + kind + "/" + url.PathEscape(strings.ToLower(id))
	response, err := c.http.Send(host.HTTPRequest{
		Method:  "GET",
		URL:     target,
		Headers: apiHeaders(),
	}, []string{"coverartarchive.org"})
	if err != nil {
		var status *shared.StatusError
		if errors.As(err, &status) && status.StatusCode == 404 {
			c.cacheResult(key, cachedImages{Found: false}, negativeTTL)
			return nil, false, nil
		}
		return nil, false, retryIfLarge(err)
	}

	var payload caaResponse
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return nil, false, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("invalid Cover Art Archive JSON")}
	}
	images := buildImageList(payload.Images)
	if len(images) == 0 {
		c.cacheResult(key, cachedImages{Found: false}, negativeTTL)
		return nil, false, nil
	}
	c.cacheResult(key, cachedImages{Found: true, Images: images}, positiveTTL)
	return images, true, nil
}

type caaResponse struct {
	Images []caaImage "json:\"images\""
}

type caaImage struct {
	Front      bool              "json:\"front\""
	Image      string            "json:\"image\""
	Types      []string          "json:\"types\""
	Thumbnails map[string]string "json:\"thumbnails\""
}

func buildImageList(entries []caaImage) []metadata.ImageInfo {
	if len(entries) == 0 {
		return nil
	}
	chosen := -1
	for i := range entries {
		if entries[i].Front || hasFrontType(entries[i].Types) {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		chosen = 0
	}

	entry := entries[chosen]
	var images []metadata.ImageInfo
	seen := map[string]struct{}{}
	for _, size := range []struct {
		key   string
		value int32
	}{{key: "250", value: 250}, {key: "500", value: 500}, {key: "1200", value: 1200}} {
		if imageURL := validCAAImageURL(entry.Thumbnails[size.key]); imageURL != "" {
			images = appendUniqueImage(images, seen, imageURL, size.value)
		}
	}
	if imageURL := validCAAImageURL(entry.Image); imageURL != "" {
		images = appendUniqueImage(images, seen, imageURL, 0)
	}
	return images
}

func hasFrontType(types []string) bool {
	for _, imageType := range types {
		if strings.EqualFold(strings.TrimSpace(imageType), "front") {
			return true
		}
	}
	return false
}

func appendUniqueImage(images []metadata.ImageInfo, seen map[string]struct{}, imageURL string, size int32) []metadata.ImageInfo {
	if _, exists := seen[imageURL]; exists {
		return images
	}
	seen[imageURL] = struct{}{}
	return append(images, metadata.ImageInfo{URL: imageURL, Size: size})
}

func validCAAImageURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return ""
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return ""
	}
	hostname := strings.ToLower(parsed.Hostname())
	switch {
	case hostname == "coverartarchive.org":
		if !strings.HasPrefix(parsed.Path, "/release/") && !strings.HasPrefix(parsed.Path, "/release-group/") {
			return ""
		}
	case hostname == "archive.org":
		if !strings.HasPrefix(parsed.Path, "/download/") && !strings.HasPrefix(parsed.Path, "/details/") {
			return ""
		}
	case strings.HasSuffix(hostname, ".archive.org"):
		// Cover Art Archive uses archive.org image hosts for CDN delivery.
	default:
		return ""
	}
	return parsed.String()
}

type releaseGroupSearch struct {
	ReleaseGroups []releaseGroup "json:\"release-groups\""
}

type releaseGroup struct {
	ID                 string          "json:\"id\""
	Title              string          "json:\"title\""
	ArtistCredit       json.RawMessage "json:\"artist-credit\""
	ArtistCreditPhrase string          "json:\"artist-credit-phrase\""
}

type artistCreditEntry struct {
	Name   string "json:\"name\""
	Artist struct {
		Name string "json:\"name\""
	} "json:\"artist\""
}

func (c *pluginClient) searchReleaseGroup(name, artist string) (string, bool, error) {
	cacheKey := "search:" + normalize(name) + "\x00" + normalize(artist)
	var cached cachedReleaseGroup
	if c.cache.GetJSON(cacheKey, &cached) {
		return cached.ID, cached.Found, nil
	}

	if !validSearchText(name) || !validSearchText(artist) {
		return "", false, nil
	}
	if err := c.cache.Throttle("musicbrainz", 1100*time.Millisecond, c.deadline, c.now, c.sleep); err != nil {
		return "", false, err
	}
	query := "releasegroup:\"" + escapeLucenePhrase(name) + "\" AND artist:\"" + escapeLucenePhrase(artist) + "\""
	params := url.Values{}
	params.Set("fmt", "json")
	params.Set("limit", "10")
	params.Set("query", query)
	requestURL := musicBrainzURL + "?" + params.Encode()
	response, err := c.http.Send(host.HTTPRequest{
		Method:  "GET",
		URL:     requestURL,
		Headers: apiHeaders(),
	}, []string{"musicbrainz.org"})
	if err != nil {
		var status *shared.StatusError
		if errors.As(err, &status) && status.StatusCode == 404 {
			c.cacheResult(cacheKey, cachedReleaseGroup{Found: false}, negativeTTL)
			return "", false, nil
		}
		return "", false, err
	}

	var payload releaseGroupSearch
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return "", false, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("invalid MusicBrainz JSON")}
	}
	var match string
	for _, candidate := range payload.ReleaseGroups {
		if !validMBID(candidate.ID) || !sameText(candidate.Title, name) || !candidateHasArtist(candidate, artist) {
			continue
		}
		if match != "" && !strings.EqualFold(match, candidate.ID) {
			// Multiple exact candidates are not safe to choose deterministically.
			c.cacheResult(cacheKey, cachedReleaseGroup{Found: false}, negativeTTL)
			return "", false, nil
		}
		match = strings.ToLower(candidate.ID)
	}
	result := cachedReleaseGroup{Found: match != "", ID: match}
	ttl := negativeTTL
	if result.Found {
		ttl = positiveTTL
	}
	c.cacheResult(cacheKey, result, ttl)
	return result.ID, result.Found, nil
}

type cachedReleaseGroup struct {
	Found bool   "json:\"found\""
	ID    string "json:\"id,omitempty\""
}

func candidateHasArtist(candidate releaseGroup, requested string) bool {
	var entries []json.RawMessage
	if err := json.Unmarshal(candidate.ArtistCredit, &entries); err == nil {
		for _, raw := range entries {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				if sameText(text, requested) {
					return true
				}
				continue
			}
			var entry artistCreditEntry
			if json.Unmarshal(raw, &entry) == nil && (sameText(entry.Name, requested) || sameText(entry.Artist.Name, requested)) {
				return true
			}
		}
	}
	return sameText(candidate.ArtistCreditPhrase, requested)
}

func escapeLucenePhrase(value string) string {
	const reserved = "+-!(){}[]^\"~*?:/\\&|"
	var out strings.Builder
	for _, r := range value {
		if strings.ContainsRune(reserved, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func validSearchText(value string) bool {
	if value == "" || len(value) > maxNameBytes {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func normalize(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func sameText(left, right string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(strings.TrimSpace(left)), " "),
		strings.Join(strings.Fields(strings.TrimSpace(right)), " "))
}

func apiHeaders() map[string]string {
	return map[string]string{
		"Accept":     "application/json",
		"User-Agent": userAgent,
	}
}

func (c *pluginClient) cacheResult(key string, result any, ttl int64) {
	_ = c.cache.SetJSON(key, result, ttl)
}

func retryIfLarge(err error) error {
	var large *shared.BodyTooLargeError
	if errors.As(err, &large) {
		return &shared.RetryError{After: 5 * time.Second, Cause: err}
	}
	return err
}

func retryLater(err error) error {
	delay := shared.RetryDelay(err)
	seconds := int((delay + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}
	return metadata.MetadataAgentError(fmt.Sprintf("agent(retry_later:%d)", seconds))
}

func main() {}
