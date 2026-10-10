# Implementation and acceptance log

Status: P0–P2 passed. P3 and CI remain in progress; P4 is deferred for manual Primuse testing.

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
- P2 behavior is locally validated. Its real OneCloud-to-Arasaka worker path and deployed runtime verification are recorded under P3 and remain in progress.

### P2 acceptance state

P2 PASS for policy, cache adapter, focused Go tests, simulated worker/write failures, and local FFmpeg/ffprobe output. Live remote-worker streaming, deployed cache persistence, and network behavior remain P3 acceptance items.

## P3 — Arasaka worker, live E2E, and CI (in progress)

### Worker and remote adapter implementation

- Added a Python 3.14 worker with `/health`, `/ready`, and authenticated `/v1/transcode`; the initial backend accepts only the signed `mp3-cbr-192-v1` profile and invokes a fixed FFmpeg argument vector.
- Source audio is uploaded as a bounded HTTP body over a pipe. Short-lived HMAC tokens bind media ID, source size/mtime, duration, offset, preset, expiry, and one-use nonce. The worker persists replay nonces in SQLite.
- Worker responses are chunked and include completion status, byte count, and SHA-256 trailers. Navidrome validates all trailers before the permanent cache can publish a completed file. Redirects and HTTP proxies are disabled; the production endpoint is constrained to `http://192.168.137.1:13324`.
- Worker concurrency is one; input size and duration are bounded; FFmpeg output is streamed with bounded logs and a timed process kill. A separate watchdog polls both HTTP routes and asks Servy to restart only `ArasakaFFmpegWorker` after repeated failures.
- Added Servy/Windows firewall deployment scripts and pinned FFmpeg provenance. The worker listener is constrained to `192.168.137.1:13324`, and its allow rule names only OneCloud source `192.168.137.11` and the worker Python executable. The key upload is temporary and ACL-restricted.

### Local validation and remaining acceptance

- Passed with Go 1.27.2 / GCC 13.3: `go test ./core/ffmpeg ./core/stream ./server/subsonic` and `go test -race ./core/stream` after adding the remote adapter.
- Passed: `python -m unittest discover -s worker/tests -v` (4 tests), covering signed streaming output, one-use/restart-persistent nonces, invalid signatures, and health/readiness. The deployment PowerShell script parses successfully.
- The remote adapter test verifies early response bytes while the source upload remains active and rejects a missing/failed completion trailer.
- Live Servy deployment, cross-host network checks, crash/recovery, and OneCloud streaming/cache E2E have not yet been accepted. CI workflow and real GitHub artifact verification are also pending.

### P3 acceptance state

P3 IN PROGRESS. Local implementation tests pass; no live-runtime PASS is claimed yet.

## Stage results

| Stage | Status | Evidence |
| --- | --- | --- |
| P0 | PASS | Audit recorded above; baseline `./core/ffmpeg` and `./core/stream` tests passed; commit `3895aa50` pushed to the fork. |
| P1 | PASS | External UUID mount and systemd units verified; Navidrome login, WebDAV CRUD/auth/symlink checks, source ACL, restart, resource use, and printer health recorded above. |
| P2 | PASS | Legacy and modern quality normalization, permanent disk cache, Range/seek, restart, source-change, cancellation and failure tests passed; real FFmpeg/ffprobe produced MP3 192 with tags. |
| P3 | IN PROGRESS | Worker and remote adapter are implemented and locally tested; live deployment and E2E remain. |
| CI | NOT STARTED | No workflow changes or Actions runs. |
| P4 | DEFERRED | Primuse manual validation is reserved for the user. |
