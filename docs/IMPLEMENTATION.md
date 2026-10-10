# Implementation and acceptance log

Status: P0–P3 passed. CI artifact verification remains in progress; P4 is deferred for manual Primuse testing.

## P0 — repository and environment baseline

### Repository

- Baseline: `497dfe25866d32e2229bcd962c38bda0e753c875` (`master`).
- Public fork: `lieyanbang/navidrome`, parent `navidrome/navidrome`.
- `origin`: `https://github.com/lieyanbang/navidrome.git`.
- `upstream`: `https://github.com/navidrome/navidrome.git`.
- Working branch: `codex/onecloud-webdav-worker`.
- Existing Git author/committer identity: `lieyanbang <l@lieyanbang.com>`; left unchanged.
- Fork description: “🎧 Your Personal Streaming Service — tuned for low-resource devices”.
- `git ls-remote origin refs/heads/master` returned the expected baseline SHA. Commit `3895aa50` was pushed to the fork branch successfully, verifying write access.
- Initial worktree had only the user-provided `docs/TASK.md` untracked. No existing files were modified during the audit.

### Toolchain and source findings

- Repository requires Go 1.27. Installed official Go 1.27.2 under the user profile; the Windows archive matched the official SHA-256 `1314008898bd40df77af4b014f777f08873dbdfbcd3d92308728ee03304fe04f`.
- WSL Ubuntu has GCC 13.3. A separate official Go 1.27.2 Linux toolchain was installed in the WSL user directory to run CGo-enabled tests without changing the Windows global toolchain.
- Windows Go tests with `CGO_ENABLED=0` pass for `core/ffmpeg`, but `core/stream` cannot compile because Navidrome's SQLite integration requires CGo. With official Go 1.27.2 and GCC 13.3 under WSL, the CGo-enabled baseline tests passed for `./core/ffmpeg` and `./core/stream`.
- OneCloud has Go 1.23.5, GCC 12.2, glibc 2.36, 981 MiB RAM, and no `ffmpeg`/`ffprobe`. The required Go 1.27 Linux/ARMv7 artifact therefore needs to be built elsewhere and tested on-device.
- Existing stream code already provides raw Range support, OpenSubsonic profile negotiation, FFmpeg abstraction, and a concurrent disk cache. Its shared cache is LRU/item-limited, so it cannot be reused for permanent MP3 storage as-is.
- The OpenSubsonic `stream` contract includes `maxBitRate`, preferred `format`, the `raw` no-transcode format, and `timeOffset`; the requested two-quality policy must normalize both legacy and client-profile paths.
- WebDAV upstream v5.17.0 is MIT-licensed and has a verified signed release. The release publishes ARMv5/ARMv6 binaries, but no ARMv7 binary; build the pinned source as Linux/ARMv7 instead.

### OneCloud baseline (read-only audit)

- Host: `192.168.137.11`, Armbian Debian 12, ARMv7.
- Data volume: `/dev/mmcblk0p1`, ext4 UUID `5307332f-8a4a-4f5a-a6ef-508b876913c7`; initially unmounted and empty except `lost+found`.
- Data mount `/srv/onecloud-data` and ports 13325/13326 were absent before deployment.
- `onecloud-printer.service` was active; its existing HTTP port 8080 returned HTTP 200 from `/health/ready`. Preserve it and its configuration.
- Root filesystem had 3.8 GiB free. No persistent firewall rules were present; the input policy was ACCEPT.

### Arasaka baseline (read-only audit)

- Host: Windows 10 IoT Enterprise LTSC 21H2, x64, LAN `192.168.137.1`.
- Servy 10.2.0 is registered as an Automatic, Running Windows service. Its CLI supports new service registration, process recovery, log rotation, and process heartbeat checks. The heartbeat does not perform HTTP GET checks, so `/health` and `/ready` need independent monitoring.
- No listener on port 13324 existed. Windows Firewall profiles were enabled with default inbound Block and outbound Allow.
- Python 3.14.6 is available; FFmpeg was not found on PATH. Existing services/listeners were inventoried and are out of scope for modification.
- The existing Printer service and OneCloud port 8080 were healthy at baseline.

### P0 acceptance state

P0 PASS. Repository identity, upstream relationship, dedicated branch, local author identity, public fork, baseline CGo tests, service audits, and an actual commit/push were verified. At the end of the read-only baseline audit, no deployment or firewall changes had been made; P1 changes and verification are recorded below.

## P1 — OneCloud Navidrome and WebDAV deployment

### Storage and systemd

- Mounted `/dev/mmcblk0p1` (ext4 UUID `5307332f-8a4a-4f5a-a6ef-508b876913c7`) at `/srv/onecloud-data`. The persistent `/etc/fstab` entry uses `noatime,nosymfollow,nofail,x-systemd.device-timeout=15s,errors=remount-ro`; live `findmnt` confirmed those options.
- Created `music`, `.cache`, `navidrome`, and `worker-state` directories on the external volume. The Navidrome DB and cache are configured on this volume. Both application units require the mount and have `ConditionPathIsMountPoint`, preventing writes to the system disk when it is absent.
- `navidrome.service`, `onecloud-webdav.service`, and `onecloud-media-firewall.service` are enabled and active. Both server units ran through a controlled `systemctl restart` and returned to service. They bind only to `192.168.137.11:13326` and `192.168.137.11:13325`, respectively.
- A first static CGo ARMv7 Navidrome binary spun before making system calls on the target. It was stopped and replaced with the normal dynamic CGo link used by the upstream Makefile. The replacement runs on the OneCloud ARMv7 CPU with its installed ARM hard-float libc; it reported ready in 12.62 seconds on first start and 395.9 ms after the initial database setup. Installed binary SHA-256: `0d12e194d43bad377bdaa7970e23f927f2e323fd0947522ca3a719aa5a1cfc43`.
- Built upstream `hacdias/webdav` v5.17.0 from signed tag commit `94efdabc9e56d365339a202c37e49449fb8c6704` as a native ARMv7 binary. Installed binary SHA-256: `19de3c465ea4846e38b96f7e7550b54cd14f3d1633e25c32c9a2e9de9659d47f`.

### Authentication, WebDAV, and network checks

- Created the first Navidrome administrator through `/auth/createAdmin`; HTTP returned 200. A separate LAN login returned 200. The generated credential is stored only in `/root/onecloud-navidrome-admin.txt` (mode `0600`).
- WebDAV uses Basic authentication, CRUD permissions, and `/srv/onecloud-data` as its complete root. Credentials are outside that root: `/etc/webdav/credentials.env` is `root:webdav 0640`, and the operator copy is `/root/onecloud-webdav-credentials.txt` (`0600`). No credentials were added to Git.
- From LAN client `192.168.137.14`, unauthenticated WebDAV returned 401; authenticated root PROPFIND returned 207 and listed both `music` and `.cache`; MKCOL, PUT, GET, MOVE, and DELETE all passed in a temporary directory that was removed. After restart, unauthenticated WebDAV again returned 401 and authenticated PROPFIND returned 207.
- A temporary symlink to `/etc/passwd` inside the WebDAV tree and an encoded `..` traversal request both returned 404; neither returned file contents. The temporary directory and symlink were removed. The ext4 mount reports `nosymfollow`.
- The dedicated `ONECLOUD_MEDIA_LAN` INPUT chain accepts only TCP 13325/13326 from `192.168.137.0/24` and drops other sources to those ports. The host INPUT policy and unrelated ports were left unchanged. LAN client authentication/CRUD succeeded. Binding the Windows client to its non-LAN Tailscale source `100.124.104.23` caused requests to each port to time out, and the firewall DROP counter increased for each probe.
- `onecloud-printer.service` remained active, and `http://127.0.0.1:8080/health/ready` returned HTTP 200 after deployment and restart. No other existing service was changed.
- At steady state, Navidrome RSS was about 37.8 MiB and WebDAV RSS about 10.5 MiB. The external volume had about 28 GiB free after initial DB/cache creation; the root filesystem remains separate from media/cache writes.

### P1 acceptance state

P1 PASS. Persistent external storage, systemd services, bind addresses, authentication, full-root WebDAV CRUD, symlink/traversal blocking, source-IP firewall behavior, service restart, and preservation of the existing printer endpoint were verified on the target device and from a LAN client.

## P2 — fixed qualities and permanent transcode cache

### Playback policy

- Compatible direct-play requests continue to return the original file, including explicit legacy `format=raw` requests. Every required transcode is normalized to MP3 at 192 kbps through both legacy Subsonic and modern OpenSubsonic decision paths; client profile, requested bitrate, server format override, and downsampling defaults cannot select another encoded quality.
- The MP3 path uses Navidrome's built-in default encoder command and explicitly passes `-abr 0` alongside `-b:a 192k` to libmp3lame. User-edited format commands cannot change this fixed profile. Raw sources retain their existing seek/range behavior.

### Cache behavior

- Added a separate `transcoding-permanent` adapter under the configured cache folder. Keys include media ID, database `UpdatedAt`, source size and nanosecond modification time, and preset `mp3-cbr-192-v1`; the source is checked before and after encoding so a concurrent source change cannot publish under an old key.
- A file-backed reader streams appended bytes to consumers while the encoder runs. Concurrent consumers share one producer. Completed output is synced and atomically renamed from `.part`; only completed files are seekable and eligible after restart. Completed entries are not removed by TTL, LRU, capacity, or startup cleanup. Startup removes only abandoned `.part` files.
- Source offsets bypass the full-song cache. Encoder-close errors, source changes, write errors including simulated ENOSPC, cancellation, empty output, and restart leftovers do not publish a final entry. The cache copies through fixed-size file I/O and does not buffer an entire song in memory.

### Validation

- Passed with Go 1.27.2 / GCC 13.3: `go test ./core/ffmpeg ./core/stream ./server/subsonic`.
- Passed: `go test -race ./core/stream`.
- Permanent-cache tests cover first-byte delivery before producer completion, concurrent dedupe, complete cache hits after constructing a new cache instance, HTTP Range 206 from completed output, source-version key invalidation, offset bypass, cancellation, startup `.part` recovery, encoder-close failure, and disk-write failure.
- Real local encoder check used FFmpeg 6.1.1 on the repository's `tests/fixtures/test.flac`; ffprobe reported `codec_name=mp3`, `bit_rate=192000`, duration `1.044898`, and preserved title metadata. The fixture output was temporary and removed.
- P2 behavior is locally validated. Its real OneCloud-to-Arasaka worker path and deployed runtime verification are recorded under P3 and passed.

### P2 acceptance state

P2 PASS for policy, cache adapter, focused Go tests, simulated worker/write failures, and local FFmpeg/ffprobe output. Live remote-worker streaming, deployed cache persistence, and network behavior remain P3 acceptance items.

## P3 — Arasaka worker, live E2E, and CI (passed)

### Worker and remote adapter implementation

- Added a Python 3.14 worker with `/health`, `/ready`, and authenticated `/v1/transcode`; the initial backend accepts only the signed `mp3-cbr-192-v1` profile and invokes a fixed FFmpeg argument vector.
- Source audio is uploaded as a bounded HTTP body over a pipe. Short-lived HMAC tokens bind media ID, source size/mtime, duration, offset, preset, expiry, and one-use nonce. The worker persists replay nonces in SQLite.
- Worker responses are chunked and include completion status, byte count, and SHA-256 trailers. Navidrome validates all trailers before the permanent cache can publish a completed file. Redirects and HTTP proxies are disabled; the production endpoint is constrained to `http://192.168.137.1:13324`.
- Worker concurrency is one; input size and duration are bounded; FFmpeg output is streamed with bounded logs and a timed process kill. A separate watchdog polls both HTTP routes and asks Servy to restart only `ArasakaFFmpegWorker` after repeated failures.
- Added Servy/Windows firewall deployment scripts and pinned FFmpeg provenance. The worker listener is constrained to `192.168.137.1:13324`, and its allow rule names only OneCloud source `192.168.137.11` and the worker Python executable. The key upload is temporary and ACL-restricted.

### Local validation and live deployment

- Passed with Go 1.27.2 / GCC 13.3: `go test ./core/ffmpeg ./core/stream ./server/subsonic` and `go test -race ./core/stream` after adding the remote adapter.
- Passed: `python -m unittest discover -s worker/tests -v` (4 tests), covering signed streaming output, one-use/restart-persistent nonces, invalid signatures, and health/readiness. The deployment PowerShell script parses successfully.
- The remote adapter test verifies early response bytes while the source upload remains active and rejects a missing/failed completion trailer.
- Deployed on Arasaka at `192.168.137.1:13324` with Servy services `ArasakaFFmpegWorker` and `ArasakaFFmpegWatchdog`, both Automatic and Running. The worker binds only to the configured LAN address. Windows Firewall rule `ArasakaFFmpegWorker-OneCloud-13324` allows inbound TCP 13324 only to `192.168.137.1`, from `192.168.137.11`, for the worker's Python executable. OneCloud reached `/health` and `/ready` with HTTP 200; a desktop source outside the allow list was denied.
- OneCloud's systemd environment points at the worker URL and root-owned secret file. The secret file is mode `0640`, owned by `root:navidrome`; no secret is stored in the repository. Remote worker concurrency and both Navidrome concurrency limits are one.
- A local Linux ARMv7 hard-float build from `65b5bc0f` was used during initial adapter testing (SHA-256 `8b461c7a5cc8b44abfa3befd3ca21d3ba2e59f60032caeb6f0fb698e16f3822a`). It was preserved as a rollback copy before installing the CI artifact.
- Installed the Linux ARMv7-A artifact from Actions run `38070572090` (source commit `794fecb3f6e62526f14a7b080dc391cda1b6e62f`) at `/usr/local/bin/navidrome`. The 71,591,912-byte binary SHA-256 is `5edc310205b82d48a53a22132073bed0584852a7939b2f9424060f81111259e4`. Local `file`/`readelf` inspection identified ELF32 ARM EABI5, hard-float ABI, CPU architecture v7-A, statically linked. Its `--version` reported the expected source commit; the existing systemd service restarted and reported ready at `192.168.137.11:13326`. The previous P3 executable remains at `/srv/onecloud-data/worker-state/navidrome-pre-ci-794fecb3` for rollback.

### Automated deployed E2E acceptance

- `scripts/e2e_worker_acceptance.py` used synthetic FLAC fixtures, Navidrome's authenticated Subsonic API, SSH health/log checks, and exact per-run fixture names. It does not print credentials. It removed both fixtures and rescanned the library at completion. The harness now requires a 900-second main fixture so crash injection has enough time to interrupt an active encode.
- All 13 checks passed: authentication and scan; worker crash restart; interrupted `.part` not published; streaming while writing permanent cache; retry completion; cache hit without a worker POST; cached Range `206`; offset bypass of full-song cache; independent second media request; cache persistence across Navidrome restart; cached and raw playback while the worker was stopped; HTTP watchdog recovery; and fixture cleanup/rescan.
- A 180-second trial completed its stream after worker recovery before the interruption threshold could be met, so the harness correctly failed that trial. Its uniquely named sources were removed and the library was rescanned. The test was rerun with the 900-second minimum; all acceptance checks then passed on the CI-built ARM artifact.
- During forced worker termination, the client had received 65,536 bytes of an expected roughly 21.6 MB output. The active `.part` existed during streaming and no partial/final cache object remained after failure. Servy restarted the process (PID changed from 7944 to 9492); a new request returned the complete stream.
- `ffprobe` verified MP3, 192,000 bit/s, 44.1 kHz, two channels, and 900.048958 seconds. The retry delivered its first byte in 155 ms and completed in 92.328 seconds. A subsequent completed-cache response returned the same 21,601,299 bytes without invoking the worker. A range request returned `206`; offset playback generated a separate response and bypassed the full-song cache.
- After a controlled Navidrome restart, the full cache object remained playable. With the worker stopped, both completed-cache playback and raw FLAC direct playback succeeded. The separate HTTP watchdog detected and recovered a worker health failure; its restart counter increased by one.
- Five complete permanent MP3 cache entries totaling about 53.3 MB remain on the external media volume by design. The latest run's two source fixtures and their scanned library records were removed; no `.part` files remained.

### CI builds and OneCloud artifact acceptance

- The upstream multi-platform pipeline was preserved as `.github/pipeline.upstream.yml.disabled`, outside GitHub's workflow discovery directory. `.github/workflows/fork-build.yml` runs Go/worker tests and builds only two executable artifacts: Windows AMD64 and Linux ARMv7-A. `DOCKER_BUILD_RECORD_UPLOAD=false` suppresses Buildx diagnostic artifacts; the final run exposed exactly the two requested artifacts.
- Actions run [38070572090](https://github.com/lieyanbang/navidrome/actions/runs/38070572090) passed on commit `794fecb3f6e62526f14a7b080dc391cda1b6e62f`. The Go subsystem tests, race test, four worker tests, both platform builds, Windows PE-format check, and QEMU ARM startup smoke test all succeeded.
- Artifact `navidrome-windows-amd64` (ID `11676741681`) downloaded successfully and was verified as a 75,130,368-byte PE32+ x86-64 executable; SHA-256 `d2b9ea307c39ff1c0f435a14acbc65fe95653da5e245c421fcf8cf4529359b29`.
- Artifact `navidrome-linux-armv7-a` (ID `11676547135`) downloaded successfully, passed file/ABI inspection, was installed on OneCloud, and passed the full deployed E2E above. The ZIP archive was 23,731,400 bytes; the executable was 71,591,912 bytes.

### Final service and resource snapshot

- OneCloud `navidrome`, `onecloud-webdav`, `onecloud-media-firewall`, and the pre-existing `onecloud-printer` systemd services were all active. Listener checks showed Navidrome on `192.168.137.11:13326`, WebDAV on `192.168.137.11:13325`, and the existing printer endpoint on `:8080`. Unauthenticated WebDAV PROPFIND returned `401`; printer readiness returned `200`.
- Arasaka `ArasakaFFmpegWorker` (PID 7496) and `ArasakaFFmpegWatchdog` (PID 1868) were Automatic and Running. Existing `ArasakaDDNS`, `MediaSTT`, and `OCI-A1-Grabber` remained Running; `CodexProxy` remained Stopped, matching the audited baseline.
- At the final sample, OneCloud reported 981 MiB RAM total, 823 MiB available, 490 MiB swap free, and 3.6 GiB free on the system filesystem. The external 30 GiB volume had 28 GiB free and used about 204 MiB, including five permanent transcode entries totaling about 53.3 MB.
- Process RSS at that sample: Navidrome 38,328 KiB, WebDAV 6,036 KiB, worker 26,406,912 bytes, and watchdog 34,000,896 bytes. Navidrome reported zero systemd restarts. All uniquely named E2E source files and `.part` files were absent after cleanup.

### P3 acceptance state

P3 PASS. Local implementation tests, actual Servy deployment, LAN isolation, CI builds, the installed OneCloud ARMv7-A artifact, streaming, persistent cache, forced-crash recovery, worker-offline fallback, HTTP watchdog recovery, and fixture cleanup all passed.

## Stage results

| Stage | Status | Evidence |
| --- | --- | --- |
| P0 | PASS | Audit recorded above; baseline `./core/ffmpeg` and `./core/stream` tests passed; commit `3895aa50` pushed to the fork. |
| P1 | PASS | External UUID mount and systemd units verified; Navidrome login, WebDAV CRUD/auth/symlink checks, source ACL, restart, resource use, and printer health recorded above. |
| P2 | PASS | Legacy and modern quality normalization, permanent disk cache, Range/seek, restart, source-change, cancellation and failure tests passed; real FFmpeg/ffprobe produced MP3 192 with tags. |
| P3 | PASS | Servy worker/watchdog deployed; 13 live E2E checks passed on the CI ARM artifact; authenticated network path, 192 kb/s MP3, permanent cache, restart, crash recovery, worker-offline playback, watchdog, and test-fixture cleanup verified. |
| CI | PASS | Actions run `38070572090` succeeded with exactly the Windows AMD64 and Linux ARMv7-A artifacts; both artifacts downloaded, and the ARM artifact was installed and exercised on OneCloud. |
| P4 | DEFERRED | Primuse manual validation is reserved for the user. |
