# Implementation and acceptance log

Status: in progress through P0 audit. P1–P3 and CI have not passed yet.

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

P0 PASS. Repository identity, upstream relationship, dedicated branch, local author identity, public fork, baseline CGo tests, service audits, and an actual commit/push were verified. No deployment or firewall changes have been made yet.

## Stage results

| Stage | Status | Evidence |
| --- | --- | --- |
| P0 | PASS | Audit recorded above; baseline `./core/ffmpeg` and `./core/stream` tests passed; commit `3895aa50` pushed to the fork. |
| P1 | NOT STARTED | No services deployed. |
| P2 | NOT STARTED | No implementation changes. |
| P3 | NOT STARTED | No worker deployment or E2E run. |
| CI | NOT STARTED | No workflow changes or Actions runs. |
| P4 | DEFERRED | Primuse manual validation is reserved for the user. |
