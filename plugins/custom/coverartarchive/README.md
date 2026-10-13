# Maintained Cover Art Archive metadata agent

This production plugin is kept separately from the upstream examples. It uses a known album MBID first, tries both release and release-group IDs, then performs a MusicBrainz release-group search using the album and artist. Search candidates must match both fields exactly after whitespace and case normalization.

Only HTTPS requests to musicbrainz.org and coverartarchive.org are declared. Plugin requests do not follow redirects. Returned art URLs are accepted only from Cover Art Archive or the archive.org image hosts used by its CDN. The result includes available 250, 500, 1200, and original-image URLs.

The plugin uses Navidrome's TTL cache. Positive entries live for 30 days, negative entries for 6 hours, and at most 64 entries of up to 32 KiB each are retained by the plugin cache index. MusicBrainz calls are paced through the shared host cache; the host exposes no atomic reservation primitive, so simultaneous independent WASM instances can still race on that pacing timestamp.

Build and package using plugins/custom/build.ps1. Source provenance and the local delta are in UPSTREAM.md; the MIT license is in LICENSE.
