package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lieyanbang/navidrome/plugins/custom/shared"
	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
)

const (
	wikidataEndpoint = "https://query.wikidata.org/sparql"
	dbpediaEndpoint  = "https://dbpedia.org/sparql"
	wikipediaUA      = "NavidromeWikimediaMetadata/1.1.1 (+https://github.com/lieyanbang/navidrome)"
	positiveTTL      = int64(30 * 24 * 60 * 60)
	negativeTTL      = int64(6 * 60 * 60)
	maxNameBytes     = 256
	maxBiography     = 12 * 1024
)

var mbidPattern = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
var wikidataEntityPattern = regexp.MustCompile("^/entity/Q[1-9][0-9]*$")
var allowedLanguages = map[string]struct{}{
	"en": {}, "de": {}, "es": {}, "fr": {}, "it": {}, "ja": {}, "zh": {},
}

type wikimediaPlugin struct{}

func init() {
	metadata.Register(&wikimediaPlugin{})
}

var (
	_ metadata.ArtistURLProvider       = (*wikimediaPlugin)(nil)
	_ metadata.ArtistBiographyProvider = (*wikimediaPlugin)(nil)
	_ metadata.ArtistImagesProvider    = (*wikimediaPlugin)(nil)
)

func (*wikimediaPlugin) GetArtistURL(request metadata.ArtistRequest) (*metadata.ArtistURLResponse, error) {
	client := newDefaultClient()
	data, found, err := client.wikidataArtist(request)
	if err != nil {
		return nil, retryLater(err)
	}
	if found && data.WikipediaURL != "" {
		return &metadata.ArtistURLResponse{URL: data.WikipediaURL}, nil
	}
	if client.language != "en" || !validArtistName(request.Name) {
		return nil, nil
	}
	fallback, found, err := client.dbpediaArtist(request.Name)
	if err != nil {
		return nil, retryLater(err)
	}
	if found && fallback.WikipediaURL != "" {
		return &metadata.ArtistURLResponse{URL: fallback.WikipediaURL}, nil
	}
	return nil, nil
}

func (*wikimediaPlugin) GetArtistBiography(request metadata.ArtistRequest) (*metadata.ArtistBiographyResponse, error) {
	client := newDefaultClient()
	data, found, err := client.wikidataArtist(request)
	if err != nil {
		return nil, retryLater(err)
	}
	if found && data.WikipediaURL != "" {
		biography, hasBio, err := client.wikipediaBiography(data.WikipediaURL)
		if err != nil {
			return nil, retryLater(err)
		}
		if hasBio {
			return &metadata.ArtistBiographyResponse{Biography: biography}, nil
		}
	}
	if client.language != "en" || !validArtistName(request.Name) {
		return nil, nil
	}
	fallback, found, err := client.dbpediaArtist(request.Name)
	if err != nil {
		return nil, retryLater(err)
	}
	if found && fallback.Biography != "" {
		return &metadata.ArtistBiographyResponse{Biography: fallback.Biography}, nil
	}
	return nil, nil
}

func (*wikimediaPlugin) GetArtistImages(request metadata.ArtistRequest) (*metadata.ArtistImagesResponse, error) {
	client := newDefaultClient()
	data, found, err := client.wikidataArtist(request)
	if err != nil {
		return nil, retryLater(err)
	}
	if !found || data.ImageURL == "" {
		return nil, nil
	}
	return &metadata.ArtistImagesResponse{
		Images: []metadata.ImageInfo{{URL: data.ImageURL, Size: 0}},
	}, nil
}

type pluginClient struct {
	http     *shared.HTTPClient
	cache    *shared.BoundedCache
	language string
	deadline time.Time
	now      func() time.Time
	sleep    func(time.Duration)
}

func newDefaultClient() *pluginClient {
	language, _ := host.ConfigGet("wikipediaLanguage")
	return newClient(nil, shared.HostCache{}, language, time.Now, time.Sleep)
}

func newClient(doer shared.HTTPDoer, cache shared.CacheBackend, language string, now func() time.Time, sleep func(time.Duration)) *pluginClient {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := now().Add(shared.DefaultTotalTimeout)
	return &pluginClient{
		http:     shared.NewHTTPClient(doer, deadline, now, sleep),
		cache:    shared.NewBoundedCache(cache, "wikimedia", 64, 24*1024),
		language: safeLanguage(language),
		deadline: deadline,
		now:      now,
		sleep:    sleep,
	}
}

type artistData struct {
	Found        bool   "json:\"found\""
	EntityURL    string "json:\"entityUrl,omitempty\""
	Label        string "json:\"label,omitempty\""
	WikipediaURL string "json:\"wikipediaUrl,omitempty\""
	ImageURL     string "json:\"imageUrl,omitempty\""
}

type cachedBiography struct {
	Found bool   "json:\"found\""
	Text  string "json:\"text,omitempty\""
}

type sparqlResponse struct {
	Results struct {
		Bindings []sparqlBinding "json:\"bindings\""
	} "json:\"results\""
}

type sparqlBinding struct {
	Artist   *sparqlValue "json:\"artist,omitempty\""
	Sitelink *sparqlValue "json:\"sitelink,omitempty\""
	Image    *sparqlValue "json:\"image,omitempty\""
	Label    *sparqlValue "json:\"label,omitempty\""
	Name     *sparqlValue "json:\"name,omitempty\""
	Wiki     *sparqlValue "json:\"wiki,omitempty\""
	Comment  *sparqlValue "json:\"comment,omitempty\""
}

type sparqlValue struct {
	Value string "json:\"value\""
}

func (c *pluginClient) wikidataArtist(request metadata.ArtistRequest) (artistData, bool, error) {
	name := strings.TrimSpace(request.Name)
	mbid := strings.ToLower(strings.TrimSpace(request.MBID))
	if len(name) > maxNameBytes || !utf8.ValidString(name) {
		return artistData{}, false, nil
	}
	if !validMBID(mbid) {
		mbid = ""
	}
	if mbid == "" && !validArtistName(name) {
		return artistData{}, false, nil
	}
	cacheKey := "wikidata:" + c.language + ":"
	if mbid != "" {
		cacheKey += "mbid:" + mbid
	} else {
		cacheKey += "name:" + normalize(name)
	}
	var cached artistData
	if c.cache.GetJSON(cacheKey, &cached) {
		return cached, cached.Found, nil
	}

	query, err := buildWikidataQuery(name, mbid, c.language)
	if err != nil {
		return artistData{}, false, nil
	}
	if err := c.cache.Throttle("wikidata", time.Second, c.deadline, c.now, c.sleep); err != nil {
		return artistData{}, false, err
	}
	response, err := c.http.Send(host.HTTPRequest{
		Method: "POST",
		URL:    wikidataEndpoint,
		Headers: map[string]string{
			"Accept":       "application/sparql-results+json",
			"Content-Type": "application/x-www-form-urlencoded",
			"User-Agent":   wikipediaUA,
		},
		Body: []byte(url.Values{"query": []string{query}}.Encode()),
	}, []string{"query.wikidata.org"})
	if err != nil {
		if isNotFound(err) {
			c.cacheResult(cacheKey, artistData{}, negativeTTL)
			return artistData{}, false, nil
		}
		return artistData{}, false, err
	}
	var payload sparqlResponse
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return artistData{}, false, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("invalid Wikidata JSON")}
	}

	candidates := make(map[string]artistData)
	for _, binding := range payload.Results.Bindings {
		if binding.Artist == nil || !validWikidataEntityURL(binding.Artist.Value) {
			continue
		}
		label := ""
		if binding.Label != nil {
			label = strings.TrimSpace(binding.Label.Value)
		}
		if name != "" && label != "" && !sameText(label, name) {
			continue
		}
		candidate := candidates[binding.Artist.Value]
		candidate.EntityURL = binding.Artist.Value
		if label != "" {
			candidate.Label = label
		}
		if binding.Sitelink != nil {
			if value := validWikipediaPageURL(binding.Sitelink.Value, c.language); value != "" {
				candidate.WikipediaURL = value
			}
		}
		if binding.Image != nil {
			if value := validWikimediaImageURL(binding.Image.Value); value != "" {
				candidate.ImageURL = value
			}
		}
		candidates[binding.Artist.Value] = candidate
	}
	if len(candidates) != 1 {
		c.cacheResult(cacheKey, artistData{}, negativeTTL)
		return artistData{}, false, nil
	}
	for _, candidate := range candidates {
		candidate.Found = candidate.WikipediaURL != "" || candidate.ImageURL != ""
		if !candidate.Found {
			c.cacheResult(cacheKey, artistData{}, negativeTTL)
			return artistData{}, false, nil
		}
		c.cacheResult(cacheKey, candidate, positiveTTL)
		return candidate, true, nil
	}
	return artistData{}, false, nil
}

func buildWikidataQuery(name, mbid, language string) (string, error) {
	var identity string
	if mbid != "" {
		literal, err := sparqlLiteral(mbid)
		if err != nil {
			return "", err
		}
		identity = "?artist wdt:P434 " + literal + " ."
	} else {
		literal, err := sparqlLiteral(name)
		if err != nil {
			return "", err
		}
		identity = "?artist rdfs:label " + literal + "@" + language + " . BIND(" + literal + "@" + language + " AS ?label)"
	}
	labelFilter := ""
	if mbid != "" {
		labelFilter = " OPTIONAL { ?artist rdfs:label ?label . FILTER(LANG(?label) = \"" + language + "\" || LANG(?label) = \"en\") }"
	}
	return "SELECT ?artist ?sitelink ?image ?label WHERE { " +
		identity +
		" OPTIONAL { ?sitelink schema:about ?artist ; schema:isPartOf <https://" + language + ".wikipedia.org/> . }" +
		" OPTIONAL { ?artist wdt:P18 ?image . }" +
		labelFilter +
		" } LIMIT 20", nil
}

func sparqlLiteral(value string) (string, error) {
	if value == "" || len(value) > maxNameBytes || !utf8.ValidString(value) {
		return "", errors.New("invalid SPARQL literal")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", errors.New("control character in SPARQL literal")
		}
	}
	escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
	return "\"" + escaped + "\"", nil
}

func validWikidataEntityURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if strings.ToLower(parsed.Hostname()) != "www.wikidata.org" {
		return false
	}
	return wikidataEntityPattern.MatchString(parsed.Path)
}

func validWikipediaPageURL(raw, language string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if strings.ToLower(parsed.Hostname()) != language+".wikipedia.org" || parsed.Port() != "" && parsed.Port() != "443" {
		return ""
	}
	if !strings.HasPrefix(parsed.Path, "/wiki/") || len(parsed.Path) <= len("/wiki/") {
		return ""
	}
	return parsed.String()
}

func validWikimediaImageURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return ""
	}
	hostname := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" && port != "443" {
		return ""
	}
	if parsed.Scheme == "http" {
		parsed.Scheme = "https"
	}
	if parsed.Scheme != "https" {
		return ""
	}
	switch hostname {
	case "commons.wikimedia.org":
		if !strings.HasPrefix(parsed.Path, "/wiki/Special:FilePath/") || len(parsed.Path) <= len("/wiki/Special:FilePath/") {
			return ""
		}
	case "upload.wikimedia.org":
		if !strings.HasPrefix(parsed.Path, "/wikipedia/commons/") {
			return ""
		}
	default:
		return ""
	}
	return parsed.String()
}

func (c *pluginClient) wikipediaBiography(pageURL string) (string, bool, error) {
	cacheKey := "biography:" + pageURL
	var cached cachedBiography
	if c.cache.GetJSON(cacheKey, &cached) {
		return cached.Text, cached.Found, nil
	}
	parsed, err := url.Parse(pageURL)
	if err != nil || validWikipediaPageURL(pageURL, c.language) == "" {
		return "", false, nil
	}
	title, err := url.PathUnescape(strings.TrimPrefix(parsed.Path, "/wiki/"))
	if err != nil || title == "" || len(title) > 1024 {
		return "", false, nil
	}
	if err := c.cache.Throttle("wikipedia", 200*time.Millisecond, c.deadline, c.now, c.sleep); err != nil {
		return "", false, err
	}
	params := url.Values{}
	params.Set("action", "query")
	params.Set("format", "json")
	params.Set("prop", "extracts")
	params.Set("exintro", "1")
	params.Set("explaintext", "1")
	params.Set("redirects", "1")
	params.Set("titles", title)
	endpoint := "https://" + c.language + ".wikipedia.org/w/api.php?" + params.Encode()
	response, err := c.http.Send(host.HTTPRequest{
		Method: "GET",
		URL:    endpoint,
		Headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": wikipediaUA,
		},
	}, []string{c.language + ".wikipedia.org"})
	if err != nil {
		if isNotFound(err) {
			c.cacheResult(cacheKey, cachedBiography{Found: false}, negativeTTL)
			return "", false, nil
		}
		return "", false, err
	}
	var payload mediaWikiExtract
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return "", false, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("invalid Wikipedia JSON")}
	}
	for _, page := range payload.Query.Pages {
		if page.Missing || strings.TrimSpace(page.Extract) == "" {
			continue
		}
		biography := clampText(page.Extract, maxBiography)
		c.cacheResult(cacheKey, cachedBiography{Found: true, Text: biography}, positiveTTL)
		return biography, true, nil
	}
	c.cacheResult(cacheKey, cachedBiography{Found: false}, negativeTTL)
	return "", false, nil
}

type mediaWikiExtract struct {
	Query struct {
		Pages map[string]mediaWikiPage "json:\"pages\""
	} "json:\"query\""
}

type mediaWikiPage struct {
	Extract string "json:\"extract\""
	Missing bool   "json:\"missing\""
}

type dbpediaData struct {
	Found        bool   "json:\"found\""
	WikipediaURL string "json:\"wikipediaUrl,omitempty\""
	Biography    string "json:\"biography,omitempty\""
}

func (c *pluginClient) dbpediaArtist(name string) (dbpediaData, bool, error) {
	if !validArtistName(name) || c.language != "en" {
		return dbpediaData{}, false, nil
	}
	cacheKey := "dbpedia:" + normalize(name)
	var cached dbpediaData
	if c.cache.GetJSON(cacheKey, &cached) {
		return cached, cached.Found, nil
	}
	literal, err := sparqlLiteral(name)
	if err != nil {
		return dbpediaData{}, false, nil
	}
	query := "SELECT ?artist ?name ?wiki ?comment WHERE { " +
		"?artist foaf:name ?name . FILTER(LANG(?name) = \"en\" && LCASE(STR(?name)) = LCASE(STR(" + literal + "))) " +
		"OPTIONAL { ?artist foaf:isPrimaryTopicOf ?wiki . FILTER(STRSTARTS(STR(?wiki), \"https://en.wikipedia.org/wiki/\")) } " +
		"OPTIONAL { ?artist rdfs:comment ?comment . FILTER(LANG(?comment) = \"en\") } } LIMIT 20"
	if err := c.cache.Throttle("dbpedia", 500*time.Millisecond, c.deadline, c.now, c.sleep); err != nil {
		return dbpediaData{}, false, err
	}
	response, err := c.http.Send(host.HTTPRequest{
		Method: "POST",
		URL:    dbpediaEndpoint,
		Headers: map[string]string{
			"Accept":       "application/sparql-results+json",
			"Content-Type": "application/x-www-form-urlencoded",
			"User-Agent":   wikipediaUA,
		},
		Body: []byte(url.Values{"query": []string{query}}.Encode()),
	}, []string{"dbpedia.org"})
	if err != nil {
		if isNotFound(err) {
			c.cacheResult(cacheKey, dbpediaData{}, negativeTTL)
			return dbpediaData{}, false, nil
		}
		return dbpediaData{}, false, err
	}
	var payload sparqlResponse
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return dbpediaData{}, false, &shared.RetryError{After: 5 * time.Second, Cause: errors.New("invalid DBpedia JSON")}
	}
	candidates := map[string]dbpediaData{}
	for _, binding := range payload.Results.Bindings {
		if binding.Artist == nil || !validDBpediaEntityURL(binding.Artist.Value) || binding.Name == nil || !sameText(binding.Name.Value, name) {
			continue
		}
		candidate := candidates[binding.Artist.Value]
		if binding.Wiki != nil {
			candidate.WikipediaURL = validWikipediaPageURL(binding.Wiki.Value, "en")
		}
		if binding.Comment != nil {
			candidate.Biography = clampText(binding.Comment.Value, maxBiography)
		}
		candidates[binding.Artist.Value] = candidate
	}
	if len(candidates) != 1 {
		c.cacheResult(cacheKey, dbpediaData{}, negativeTTL)
		return dbpediaData{}, false, nil
	}
	for _, candidate := range candidates {
		candidate.Found = candidate.WikipediaURL != "" || candidate.Biography != ""
		if !candidate.Found {
			c.cacheResult(cacheKey, dbpediaData{}, negativeTTL)
			return dbpediaData{}, false, nil
		}
		c.cacheResult(cacheKey, candidate, positiveTTL)
		return candidate, true, nil
	}
	return dbpediaData{}, false, nil
}

func validDBpediaEntityURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if strings.ToLower(parsed.Hostname()) != "dbpedia.org" {
		return false
	}
	return strings.HasPrefix(parsed.Path, "/resource/") && len(parsed.Path) > len("/resource/")
}

func safeLanguage(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if _, ok := allowedLanguages[value]; ok {
		return value
	}
	return "en"
}

func validMBID(value string) bool {
	return mbidPattern.MatchString(value)
}

func validArtistName(value string) bool {
	if value == "" || len(value) > maxNameBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return strings.TrimSpace(value) != ""
}

func normalize(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func sameText(left, right string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(strings.TrimSpace(left)), " "),
		strings.Join(strings.Fields(strings.TrimSpace(right)), " "))
}

func clampText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

func isNotFound(err error) bool {
	var status *shared.StatusError
	return errors.As(err, &status) && status.StatusCode == 404
}

func (c *pluginClient) cacheResult(key string, result any, ttl int64) {
	_ = c.cache.SetJSON(key, result, ttl)
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
