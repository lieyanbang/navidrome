# Maintained Wikimedia metadata agent

This production plugin adapts the official Go example at plugins/examples/wikimedia without changing that upstream sample. Wikidata is queried first by a validated MusicBrainz artist MBID or an exact configured-language label. A result is accepted only when the entity, label, Wikipedia page, and image URL pass validation and the candidate is unambiguous. DBpedia is used as a verified English fallback for the artist URL and biography.

Wikipedia defaults to English. The only configurable language values are en, de, es, fr, it, ja, and zh; every matching Wikipedia API host is declared exactly in the HTTP permission. SPARQL literals reject control characters and escape quotes and backslashes. HTTP responses and total lookup time are bounded, redirects are disabled, and transient provider failures return the PDK retry-later signal.

Wikidata entity/page results and biography responses share Navidrome's TTL cache across URL, biography, and image calls. Positive entries live for 30 days, negative entries for 6 hours, with at most 64 entries of up to 24 KiB each. Cache and rate-limit operations use the host service; its API has no atomic compare-and-swap, so simultaneous independent WASM instances can still race on a pacing timestamp.
