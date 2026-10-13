# Wikimedia source provenance

- Base implementation: official Go example in plugins/examples/wikimedia.
- Fork base revision: 1188b82bf2e2fab8e2b42fc5b4d08d7f6fceba7d.
- The official example remains unchanged. This production implementation is maintained separately under plugins/custom/wikimedia and uses the PDK from this revision.
- The upstream Navidrome repository is licensed under AGPL-3.0-or-later; this derivative remains under the repository license.

Local changes:

- Strict SPARQL escaping and validation for artist names, MBIDs, language, Wikidata entities, Wikipedia pages, and image URLs.
- Exact label verification and ambiguity rejection for Wikidata and DBpedia candidates.
- URL, biography, and image lookups share bounded positive and negative host-cache entries.
- One total deadline, per-request timeout, response-size limit, redirect rejection, one bounded gateway retry, provider pacing, and transient retry-later results.
- Host-mocked tests cover all three capabilities, fallbacks, invalid input, malformed responses, rate limits, cache reuse, and candidate ambiguity.
