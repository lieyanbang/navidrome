# Navidrome metadata plugin acceptance

Date: 2026-10-13 UTC
Branch: `codex/onecloud-webdav-worker`
OneCloud: `192.168.137.11` (Debian 12, ARMv7)

## Phase status

| Phase | Status | Evidence |
|---|---|---|
| 0 — Audit and baseline | PASS | Recorded branch, dirty user files, upstream, PDK/ABI, ARMv7 runtime, mounted data volume, service PIDs/ports, empty plugin folder, and effective agent defaults before changes. |
| 1 — Cover Art Archive | PASS | Maintained Go source imported with MIT attribution and pinned upstream revision; MBID-first and exact-name fallback, bounded HTTP/cache behavior, safe URLs, front-art preference, and deterministic provider tests. |
| 2 — Wikimedia | PASS | New production Go plugin derived from the unchanged official example; safe SPARQL literals and URL validation, exact candidate selection, shared bounded cache, English default, fallback, and deterministic host-mocked tests. |
| 3 — Validate and build | PASS | Formatting, tests, vet, race tests, tagged root regressions, reproducible WASI builds, package inspection, and real Extism/Wazero export smoke all passed. ARMv7 Navidrome loaded both packages. |
| 4 — Deploy and activate | BLOCKED | Both 1.1.1 plugins are installed, enabled, loaded, and healthy. Continuous playback was not proven: the UI had a one-item player badge before restart; afterward the server queue table was empty, although a player heartbeat remained recent. No playback or queue edits were made during validation. |
| 5 — Final acceptance | BLOCKED | Wikimedia returned and persisted a Wikipedia URL, biography, and Commons image for an existing artist. The installed CAA plugin was invoked for an existing album, but the provider path backed off and no cover was persisted. The user-authenticated UI was available, so no login-dependent check was skipped. |

## Source, build, and tests

- Cover Art Archive source is maintained under `plugins/custom/coverartarchive`; upstream Go revision: `0f228002eff4c2a2c03b693216e5775f95eeab04`. The MIT license and attribution are retained. The official Python example and official Wikimedia example remain unchanged.
- Shared HTTP/cache safety code is under `plugins/custom/shared`; the Wikimedia implementation is under `plugins/custom/wikimedia`; the deterministic package builder is `plugins/custom/build.ps1`; the real WASM host smoke is `plugins/custom/smoke/main.go`.
- Build command: `pwsh.exe -NoProfile -File .\plugins\custom\build.ps1`. It pins Go 1.27.2, builds each WASI module twice, compares package hashes, and checks the two-entry `.ndp` layout.
- `go test -count=1 ./...` and `go vet ./...` passed in the shared, Cover Art Archive, and Wikimedia modules. `go test -race -count=1 ./...` passed in all three modules under WSL. The broader command `CGO_ENABLED=1 go test -tags netgo,sqlite_fts5 ./plugins/... ./core/agents/...` passed.
- The packaged exports ran through the real generated Navidrome host functions and Extism/Wazero with deterministic fake HTTP/cache data. One hundred CAA export calls used one mock HTTP request: cold 16.63 ms, warm median 5.41 ms, warm p95 8.12 ms. Wikimedia URL, biography, and image exports shared two mock HTTP requests. These timings are mocked-host measurements, not live provider latency.

| Package | Version | Size | SHA-256 | Entries |
|---|---:|---:|---|---|
| `outputs/plugin-metadata-20261013/artifacts/coverartarchive.ndp` | 1.1.1 | 5,716,051 bytes | `d64e5432b1cc9cab2b3daecc467965fe678d7706793e522159437907953b3066` | `manifest.json`, `plugin.wasm` |
| `outputs/plugin-metadata-20261013/artifacts/wikimedia.ndp` | 1.1.1 | 5,748,006 bytes | `99bbb9259880cb2d2ec2e74b0389b47a530c13ff975a337df3c7107c953d0b8c` | `manifest.json`, `plugin.wasm` |

## OneCloud deployment

- `/srv/onecloud-data` was mounted read-write before writes. The packages are installed in `/srv/onecloud-data/navidrome/plugins`, owned by `navidrome:navidrome`, mode `0640`. The live hashes match the table above.
- Navidrome reports both plugins enabled at version 1.1.1 with `cache,http` permissions, no write access, and no plugin load error. Startup logs report both `MetadataAgent` capabilities loaded. The effective agent order is `coverartarchive,wikimedia,local`; existing local metadata behavior remains enabled.
- `/etc/navidrome/navidrome.env` has only two task additions: `ND_PLUGINS_ENABLED=true` and `ND_AGENTS=coverartarchive,wikimedia,local`. The original file is backed up at `/srv/onecloud-data/navidrome/backups/navidrome.env.pre-plugin-metadata-20261013` (mode `0600`, SHA-256 `9085b862275435a6a4aa03f4e015a6d927e9b5f2eab65f013ef5dd51ef9403de`). The previous 1.1.0 packages are backed up under `/srv/onecloud-data/navidrome/backups/plugins-before-1.1.1/`.
- Navidrome was gracefully restarted to load configuration and then the updated 1.1.1 packages. Final PID: `25097`. WebDAV PID `27423` and printer PID `8570` did not change. Navidrome is active, `/ping` succeeds, listeners `13326`, `13325`, and `8080` remain present, and `192.168.137.1:13324` is reachable from OneCloud. The Navidrome core binary was not replaced.
- RAM total was 1,004,892 KiB. Final Navidrome RSS was 351,260 KiB, available RAM 524,288 KiB. A 15-second idle sample measured 0.3% process CPU with stable RSS. No OOM events were found. The Navidrome cache tree is 161 MiB; four files totaling 126,501 bytes were written or updated during the task window. The mounted data volume still had 19 GiB available.
- A warning that artwork settings differ from the last full reprocess remains. No full artwork reprocess was run, so existing artwork files/caches were not bulk-regenerated. Player heartbeat was present in the recent window, but `playqueue` had zero rows after restart; uninterrupted playback is therefore unverified.

## Live metadata result and limitation

- Through the existing authenticated UI, the Wikimedia agent refreshed an existing `Bebe Rexha` artist. The database now has a biography (1,860 characters), an `en.wikipedia.org` external URL, and a `commons.wikimedia.org` image URL. The biography remained visible in the UI after the final Navidrome restart.
- The CAA agent was invoked for the existing `Ferrari` album, which has no stored MusicBrainz ID. The album still has no external image or `external_info_updated_at`. A single read-only MusicBrainz probe returned HTTP 503 with `Retry-After: 0`; the plugin manager also recorded provider-backoff circuit-breaker warnings. The shared retry parser was hardened so non-positive `Retry-After` values use the 5-second conservative default instead of a one-second retry. The final live attempt still received provider backoff, so CAA live image acceptance remains blocked by the external provider state.
- No severe Navidrome errors or OOM events were observed. Four provider-backoff notifications occurred during the final validation window; no broad retry loop was started.

## Recovery

To restore the previous runtime, restore the environment backup to `/etc/navidrome/navidrome.env` with owner `root:navidrome` and mode `0640`; restore both `.ndp` files from `backups/plugins-before-1.1.1/` to the plugin directory with owner `navidrome:navidrome` and mode `0640`; run `navidrome plugin rescan`, re-enable both plugin IDs if the rescan marks changed files disabled, then restart only `navidrome.service`.

Unrelated pre-existing edits and untracked user files, including the existing `docs/IMPLEMENTATION.md` appendix, `docs/TASK.md`, and `outputs/lan-access-20261013/`, were preserved.
