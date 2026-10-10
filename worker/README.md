# Arasaka FFmpeg Worker

The worker accepts only authenticated, one-use source streams from OneCloud and produces fixed MP3 192 kbps CBR. It never receives a source URL, path, FFmpeg argument, Navidrome credential, or client-provided encoder setting. The caller signs the media identity, source size/modification fingerprint, duration, offset, preset, expiry, and nonce with the shared HMAC key.

The versioned API is `GET /health`, `GET /ready`, and `POST /v1/transcode`. Transcode responses are HTTP/1.1 chunked streams whose final trailers include `X-Transcode-Status`, an SHA-256 digest, and byte count. Only `complete` plus matching length and digest is a successful stream. A failed or disconnected encoder does not signal completion.

The initial encoder backend is FFmpeg/libmp3lame. The backend boundary is the `FFmpegBackend.run` method; future encoders can implement the same bounded streaming contract without changing the token, HTTP, replay-protection, or watchdog modules.

Configuration is through `ARASAKA_WORKER_*` environment variables. Defaults bind only to `192.168.137.1:13324`, read the key outside the WebDAV volume, limit input to 2 GiB, duration to six hours, and concurrency to one. The SQLite nonce ledger is stored outside media directories and persists across worker restarts. Windows Firewall must restrict inbound TCP 13324 to `192.168.137.11`.

`python -m unittest discover -s worker/tests -v` runs the standard-library protocol and token tests. `python -m worker.arasaka_worker.server` runs the server; `python -m worker.arasaka_worker.watchdog` actively checks both HTTP health endpoints and asks Servy to restart only `ArasakaFFmpegWorker` after repeated failures.
