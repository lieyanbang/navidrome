# Maintained Cover Art Archive metadata agent

This production plugin is kept separately from the upstream examples. It uses a known album MBID first, tries both release and release-group IDs, then performs a MusicBrainz release-group search using the album and artist. Search candidates must match both fields exactly after whitespace and case normalization.

Only HTTPS requests to MusicBrainz, Cover Art Archive, and archive.org hosts are declared. Redirects remain disabled by default. For a release MBID, the plugin follows only a Cover Art Archive redirect to that release's archive index. For a release-group MBID, it also accepts the valid member-release MBID that Cover Art Archive maps the group to. It then follows at most one validated archive CDN redirect. If the download route returns a retryable failure, the plugin queries Internet Archive item metadata and tries at most two validated mirror hosts for that same `index.json`. Returned art URLs are accepted only from Cover Art Archive or archive.org image hosts used by its CDN. The result includes available 250, 500, 1200, and original-image URLs.

The plugin also implements `GetAlbumInfo` so Navidrome's external metadata refresh can proceed to `GetAlbumImages`. It returns a known release MBID directly or performs the same exact-title-and-artist MusicBrainz release-group match when the album has no MBID.

The plugin uses Navidrome's TTL cache. Positive entries live for 30 days, negative entries for 6 hours, and at most 64 entries of up to 32 KiB each are retained by the plugin cache index. MusicBrainz calls are paced through the shared host cache; the host exposes no atomic reservation primitive, so simultaneous independent WASM instances can still race on that pacing timestamp.

Build and package using plugins/custom/build.ps1. Source provenance and the local delta are in UPSTREAM.md; the MIT license is in LICENSE.
