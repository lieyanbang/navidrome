# Navidrome metadata plugin acceptance

Observed: 2026-10-13 10:29 UTC
Branch: codex/onecloud-webdav-worker
OneCloud: 192.168.137.11 (Debian 12, ARMv7)
Repository base: d13dbb3016eb69a14d1fd9b3a3a1557f6031dd2f

## Phase status

| Phase | Status | Evidence |
|---|---|---|
| 0 — Audit and baseline | PASS | Verified dirty files and branch before changes, pinned upstream CAA revision, PDK/ABI and ARMv7 host support, mounted data volume, active services, config, plugin directory, agent order, toolchain, and existing plugin packages. |
| 1 — Cover Art Archive | PASS | Maintained Go source with MIT attribution; added album-info capability, validated release/release-group identifiers and redirects, and a bounded Internet Archive metadata/mirror fallback for transient download failures. Deterministic provider and security tests pass. |
| 2 — Wikimedia | PASS | Maintained plugin is derived from the unchanged official example. Existing URL, biography, image, SPARQL escaping, matching, bounded cache, and failure-path test coverage pass. Live artist metadata was persisted on OneCloud. |
| 3 — Validate and build | PASS | Go tests, vet, race tests, broader tagged host regressions, reproducible WASI packaging, package inspection, and Extism/Wazero host smoke passed. Final smoke used the packaged 1.1.3 CAA and 1.1.1 Wikimedia artifacts. |
| 4 — Deploy and activate | PARTIAL | Both final packages match the deployed hashes, are enabled and loaded with the intended agent order, and service health is good. Continuous client playback after the restart was not verified. |
| 5 — Final acceptance | PARTIAL | Wikimedia metadata persisted. The final CAA source-level live probe returned four image URLs, but a final authenticated metadata refresh through the installed 1.1.3 plugin could not be run; that exact check is SKIPPED_LOGIN_REQUIRED, not PASS. |

## Why the plugins appeared ineffective

- Wikimedia did run: the existing Bebe Rexha record has a 1,860-character biography, an English Wikipedia URL, a Commons image URL, and item_artwork.source=external:wikimedia. This agent supplies artist metadata; it does not supply album covers. Artwork priority or cached display can also make a successful artist refresh less visible.
- CAA 1.1.1 exposed only album images. Navidrome asks an external metadata provider for album info first and returns early when it is not found, so that version never reached its image method. Version 1.1.2 added the missing album-info capability and the authenticated UI refresh stored a MusicBrainz release-group URL for Ferrari.
- That 1.1.2 refresh still produced no cover: the CAA release-group response redirected to a member release and the implementation rejected that valid mapping; download paths also showed transient upstream failures. Version 1.1.3 validates the mapping and has a bounded metadata/mirror fallback. The final deployed 1.1.3 authenticated refresh remains unverified because the available browser session showed the login form.

## Source, build, and tests

- CAA upstream revision: 0f228002eff4c2a2c03b693216e5775f95eeab04; MIT license and attribution are retained. The official Python CAA example and official Wikimedia example remain unchanged.
- Maintained source: plugins/custom/coverartarchive, plugins/custom/wikimedia, and shared host helpers in plugins/custom/shared.
- Reproducible build: pwsh.exe -NoProfile -File .\plugins\custom\build.ps1 with Go 1.27.2; each WASI module was built twice and package hashes matched.
- Final commands passed:
  - go test -count=1 ./..., go vet ./..., and go test -race -count=1 ./... in the shared, CAA, and Wikimedia modules under WSL.
  - CGO_ENABLED=1 go test -tags netgo,sqlite_fts5 ./plugins/... ./core/agents/....
  - Packaged WASM smoke through Navidrome host imports and Extism/Wazero.
- Final mock smoke: 100 CAA calls made one mock HTTP request; cold 18.01 ms, warm median 5.99 ms, warm p95 9.65 ms. Wikimedia URL, biography, and image exports shared two mock HTTP requests. These are mocked-host timings, not provider latency.
- A temporary live source-level probe (removed after use) called the final CAA provider logic against the live CAA/Internet Archive endpoints; it resolved four image URLs in 3.21 seconds. This was not an invocation through the deployed Navidrome plugin host.

| Package | Version | Size | SHA-256 | Entries |
|---|---:|---:|---|---|
| outputs/plugin-metadata-20261013/artifacts/coverartarchive.ndp | 1.1.3 | 5,747,589 bytes | fb11bad5bee91db320f63f6faac480baaf2dcb395625335cf99dc7d74298db52 | manifest.json, plugin.wasm |
| outputs/plugin-metadata-20261013/artifacts/wikimedia.ndp | 1.1.1 | 5,749,363 bytes | 175a9a37c9b962543925eb7047939a953b86591adca673f6d282332771fd71ea | manifest.json, plugin.wasm |

## OneCloud deployment and health

- Data volume /srv/onecloud-data was mounted read-write. Plugin directory: /srv/onecloud-data/navidrome/plugins. Both packages are owned by navidrome:navidrome, mode 0640; installed SHA-256 hashes match the table.
- Current plugin list: coverartarchive 1.1.3 enabled=true, wikimedia 1.1.1 enabled=true, neither has a last error. Effective order is coverartarchive,wikimedia,local; permissions are cache,http. Environment additions remain ND_PLUGINS_ENABLED=true and ND_AGENTS=coverartarchive,wikimedia,local.
- Navidrome is active, PID 6025; http://192.168.137.11:13326/ping returned HTTP 200. The service binds the LAN address, so a loopback-only probe to 127.0.0.1:13326 fails by design; no service change was needed.
- Listeners: Navidrome 13326, WebDAV 13325 (PID 27423), printer 8080 (PID 8570). The Arasaka worker 192.168.137.1:13324 is reachable. WebDAV and printer PIDs remained unchanged through deployment.
- Final 15-second idle sample: Navidrome RSS stayed at 259,352 KiB; process CPU was 0.33%. Total RAM 1,004,892 KiB, available RAM 623,520 KiB; data volume had 18.77 GiB free. No kernel OOM events and no Navidrome error-level journal entries were found in the checked windows.
- No full artwork reprocess was run. Continuous playback after the graceful Navidrome restart remains unverified; validation did not change the player queue or start playback.

## 503 diagnosis and fallback

The observed CAA release-group request returned a 307 to a member-release archive item. Internet Archive /download returned a transient 503 in the same validation window; its metadata endpoint then returned the item directory and mirror names, and the validated index/image mirrors returned 200. CAA 1.1.3 uses that metadata route and at most two allow-listed archive mirrors after retryable download failures. The official CAA API documents redirect/index behavior and release-group metadata ([Cover Art Archive API](https://musicbrainz.org/doc/Cover_Art_Archive)); the Internet Archive documents /metadata/{identifier} and its d1, d2, and dir fields ([Item Metadata API: Read](https://doc-tools.readthedocs.io/en/ia-test-gsod/md-read.html)).

If the Internet Archive metadata API and its mirrors also fail persistently, TheAudioDB is an independent candidate, not a deployed fallback. Its free API documents album search by artist/title and a 30-request-per-minute limit, and its shared routing can itself return 503 ([API documentation](https://www.theaudiodb.com/free_music_api)). Its terms require source attribution for custom artwork and restrict third-party content ([terms](https://www.theaudiodb.com/docs_terms_of_use.php)). It was not added because the in-scope IA metadata/mirror fallback succeeded in the live source probe and the task is limited to the two requested plugins.

## Backups and recovery

- Original packages: /srv/onecloud-data/navidrome/backups/plugins-before-metadata-fix-20261013/.
- Packages immediately before CAA 1.1.3: /srv/onecloud-data/navidrome/backups/plugins-before-caa-mirror-fix-20261013/ (CAA 1.1.2 and Wikimedia 1.1.1).
- Environment backup: /srv/onecloud-data/navidrome/backups/navidrome.env.pre-plugin-metadata-20261013.
- To roll back only CAA, restore its 1.1.2 backup into the plugin directory as coverartarchive.ndp, retain the current Wikimedia package, run navidrome plugin rescan, explicitly enable coverartarchive if rescan disabled the changed package, and gracefully restart only navidrome.service. Restore the environment backup only if reverting the plugin enablement/agent settings too.

Unrelated user changes were preserved: the 10-11 appendix in docs/IMPLEMENTATION.md, untracked docs/TASK.md, and outputs/lan-access-20261013/.
