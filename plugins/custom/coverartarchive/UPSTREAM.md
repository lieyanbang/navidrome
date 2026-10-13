# Cover Art Archive source provenance

- Upstream: sunsetroute1/navidrome-coverartarchive-plugin
- Imported revision: 0f228002eff4c2a2c03b693216e5775f95eeab04
- License: MIT; the upstream copyright and license are retained in LICENSE.
- The upstream main.go, manifest.json, go.mod, and go.sum were reviewed.
- The maintained implementation is adapted to this fork's current plugins/pdk/go module and keeps the community provider's MusicBrainz release-group fallback and Cover Art Archive response mapping.

Local changes:

- Strict UUID validation for MBIDs and safe Lucene phrase escaping.
- Exact title and artist verification; ambiguous or weak MusicBrainz candidates are rejected.
- Front artwork selection and validated 250, 500, 1200, and original URLs.
- Total and per-request deadlines, response and request size limits, redirect rejection, bounded retries, MusicBrainz pacing, and provider-specific error handling.
- Positive and negative host-cache entries with TTLs and a 64-entry, 32 KiB-per-entry bound.
- Deterministic host-mocked tests and a WASM smoke path using a fake host.
- Navidrome `GetAlbumInfo` support to unlock the subsequent album-image refresh call.
- A strict, bounded exception for the official Cover Art Archive release redirect through the matching Internet Archive index and CDN.
- Release-group redirects may map to a verified member-release MBID; retryable Internet Archive download failures can fall back to at most two mirror hosts discovered through the item metadata API.
