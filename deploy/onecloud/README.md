# OneCloud deployment

These files configure the explicitly authorized OneCloud host at `192.168.137.11`.
The storage bootstrap validates the data-volume UUID before mounting or writing to it.
Both application services require that mount and check that it is an active mount point;
they therefore fail closed instead of creating library, database, or cache files on `/`.

`bootstrap-storage.sh` adds a single UUID-based `/etc/fstab` entry with `nosymfollow`,
creates the media service accounts, and prepares the volume layout. The dedicated
firewall unit adds only two INPUT-chain rules for TCP 13325 and 13326: accept sources in
`192.168.137.0/24`, then drop other sources to those ports. It leaves the INPUT policy
and every unrelated port unchanged.

The WebDAV configuration exposes `/srv/onecloud-data` as its complete root with CRUD
permissions and an environment-backed Basic Auth credential. Keep
`/etc/webdav/credentials.env` root-owned and mode `0640` with group `webdav`; do not put
credentials in this repository or on the data volume. The volume's setgid `navidrome`
group and the WebDAV account's supplementary group allow authenticated WebDAV clients
to manage the same files without running the server as root.

Navidrome binds only to `192.168.137.11:13326`. Its music library, database, and cache
live on the mounted volume. The user account is non-root, and the service uses a
restrictive systemd sandbox. HTTP is used only on the trusted LAN as specified; it does
not provide transport encryption.

Acceptance requires checking `findmnt`, both service units, their exact listen
addresses, the existing printer health endpoint on port 8080, WebDAV authentication
and PROPFIND/GET/PUT/MOVE/DELETE, root visibility of `music` and `.cache`, and an
unauthenticated source denied by the firewall. WebDAV mutation tests use only a
dedicated temporary subdirectory and remove it after verification.
