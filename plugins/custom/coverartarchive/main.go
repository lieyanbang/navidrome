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
	userAgent      = "NavidromeCoverArtArchive/1.1.3 (+https://github.com/lieyanbang/navidrome)"
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
var _ metadata.AlbumInfoProvider = (*caaPlugin)(nil)

func (*caaPlugin) GetAlbumInfo(request metadata.AlbumRequest) (*metadata.AlbumInfoResponse, error) {
	return newClient(nil, shared.HostCache{}, time.Now, time.Sleep).getAlbumInfo(request)
}

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

func (c *pluginClient) getAlbumInfo(request metadata.AlbumRequest) (*metadata.AlbumInfoResponse, error) {
	name := strings.TrimSpace(request.Name)
	artist := strings.TrimSpace(request.Artist)
	mbid := strings.ToLower(strings.TrimSpace(request.MBID))
	if len(name) > maxNameBytes || len(artist) > maxNameBytes {
		return nil, nil
	}
	if validMBID(mbid) {
		return &metadata.AlbumInfoResponse{
			Name: name,
			MBID: mbid,
			URL:  "https://musicbrainz.org/release/" + mbid,
		}, nil
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
	return &metadata.AlbumInfoResponse{
		Name: name,
		MBID: releaseGroupID,
		URL:  "https://musicbrainz.org/release-group/" + releaseGroupID,
	}, nil
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
	target += "/"
	response, err := c.http.Send(host.HTTPRequest{
		Method:  "GET",
		URL:     target,
		Headers: apiHeaders(),
	}, []string{"coverartarchive.org"})
	if err != nil && (kind == "release" || kind == "release-group") {
		var redirect *shared.RedirectError
		if errors.As(err, &redirect) {
			response, err = c.followCAAIndexRedirect(kind, id, redirect.Location)
		}
	}
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

func (c *pluginClient) followCAAIndexRedirect(kind, requestedID, location string) (*host.HTTPResponse, error) {
	archiveID, firstURL, err := parseCAAArchiveIndexURL(kind, requestedID, location)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Send(host.HTTPRequest{
		Method:  "GET",
		URL:     firstURL,
		Headers: apiHeaders(),
	}, []string{"archive.org"})
	if err == nil {
		return response, nil
	}
	var redirect *shared.RedirectError
	if errors.As(err, &redirect) {
		mirrorURL, mirrorHost, validationErr := parseArchiveMirrorURL(archiveID, redirect.Location)
		if validationErr != nil {
			return nil, validationErr
		}
		response, err = c.http.Send(host.HTTPRequest{
			Method:  "GET",
			URL:     mirrorURL,
			Headers: apiHeaders(),
		}, []string{mirrorHost})
		if err == nil {
			return response, nil
		}
	}
	if isTemporaryFailure(err) {
		return c.archiveMetadataIndex(archiveID)
	}
	return nil, err
}

func parseCAAArchiveIndexURL(kind, requestedID, location string) (string, string, error) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme != "https" || strings.ToLower(parsed.Hostname()) != "archive.org" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(location, "#") {
		return "", "", rejectedRedirect("Cover Art Archive returned an unexpected release index redirect")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return "", "", rejectedRedirect("Cover Art Archive release index redirect used a disallowed port")
	}
	const prefix, suffix = "/download/mbid-", "/index.json"
	if !strings.HasPrefix(parsed.Path, prefix) || !strings.HasSuffix(parsed.Path, suffix) {
		return "", "", rejectedRedirect("Cover Art Archive returned an unexpected release index path")
	}
	archiveID := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(parsed.Path, prefix), suffix))
	requestedID = strings.ToLower(requestedID)
	if !validMBID(archiveID) || (kind == "release" && archiveID != requestedID) {
		return "", "", rejectedRedirect("Cover Art Archive returned an unexpected release ID")
	}
	return archiveID, "https://archive.org/download/mbid-" + archiveID + "/index.json", nil
}

func parseArchiveMirrorURL(id, location string) (string, string, error) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(location, "#") {
		return "", "", rejectedRedirect("Internet Archive returned an unexpected release index redirect")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return "", "", rejectedRedirect("Internet Archive release index redirect used a disallowed port")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if !validArchiveMirrorHost(hostname) || !validArchiveIndexPath(parsed.Path, id) {
		return "", "", rejectedRedirect("Internet Archive returned an untrusted release index redirect")
	}
	return parsed.String(), hostname, nil
}

func (c *pluginClient) archiveMetadataIndex(id string) (*host.HTTPResponse, error) {
	response, err := c.http.Send(host.HTTPRequest{
		Method:  "GET",
		URL:     "https://archive.org/metadata/mbid-" + id,
		Headers: apiHeaders(),
	}, []string{"archive.org"})
	if err != nil {
		return nil, err
	}
	var payload archiveMetadataResponse
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return nil, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("invalid Internet Archive metadata JSON")}
	}
	if !validArchiveIndexDir(payload.Dir, id) || !hasArchiveIndexFile(payload.Files) {
		return nil, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("Internet Archive metadata did not identify the expected index.json")}
	}

	var lastErr error
	seen := map[string]struct{}{}
	attempts := 0
	for _, candidate := range []string{payload.D1, payload.D2, payload.Server} {
		hostname := validArchiveMetadataHost(candidate)
		if hostname == "" {
			continue
		}
		if _, exists := seen[hostname]; exists {
			continue
		}
		seen[hostname] = struct{}{}
		attempts++
		mirrorURL := "https://" + hostname + payload.Dir + "/index.json"
		mirrorResponse, mirrorErr := c.http.Send(host.HTTPRequest{
			Method:  "GET",
			URL:     mirrorURL,
			Headers: apiHeaders(),
		}, []string{hostname})
		if mirrorErr == nil {
			return mirrorResponse, nil
		}
		lastErr = mirrorErr
		if attempts == 2 {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("Internet Archive metadata contained no usable mirror host")}
}

func isTemporaryFailure(err error) bool {
	var retry *shared.RetryError
	return errors.As(err, &retry)
}

func validArchiveIndexPath(path, id string) bool {
	for _, prefix := range []string{"/0/items/mbid-", "/2/items/mbid-"} {
		if path == prefix+id+"/index.json" {
			return true
		}
	}
	return false
}

func validArchiveIndexDir(path, id string) bool {
	suffix := "/items/mbid-" + id
	if !strings.HasSuffix(path, suffix) {
		return false
	}
	shard := strings.TrimPrefix(strings.TrimSuffix(path, suffix), "/")
	if shard == "" {
		return false
	}
	for _, r := range shard {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validArchiveMirrorHost(host string) bool {
	host = strings.ToLower(host)
	return host != "" && host != "archive.org" && strings.HasSuffix(host, ".archive.org")
}

func validArchiveMetadataHost(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse("https://" + raw)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" {
		return ""
	}
	hostname := strings.ToLower(parsed.Hostname())
	if !validArchiveMirrorHost(hostname) || !strings.EqualFold(parsed.Host, hostname) {
		return ""
	}
	return hostname
}

func hasArchiveIndexFile(files []archiveMetadataFile) bool {
	for _, file := range files {
		if file.Name == "index.json" {
			return true
		}
	}
	return false
}

type archiveMetadataResponse struct {
	D1     string                `json:"d1"`
	D2     string                `json:"d2"`
	Server string                `json:"server"`
	Dir    string                `json:"dir"`
	Files  []archiveMetadataFile `json:"files"`
}

type archiveMetadataFile struct {
	Name string `json:"name"`
}

func rejectedRedirect(message string) error {
	return &shared.RetryError{After: 5 * time.Second, Cause: errors.New(message)}
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
