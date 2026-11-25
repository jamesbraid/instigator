# Changelog

Each release section opens with a one-sentence summary, then lists Breaking
(each with its migration), Features, Changed and Fixes, leaving out empty
ones. Entries for 0.5.1 and earlier were rewritten from the original GitHub release notes. For 0.2.0, 0.4.1 and 0.4.2 those were commit-subject lists, so those sections say no more than the subjects do. Release binaries are for Linux and macOS (amd64 and arm64) and Windows (amd64).

## Unreleased

Features:
- configs: an IRIX 5.3 config for the Indigo, Indy and Indigo2 and an IRIX 6.2 config for the R4000 Indigo, Indy and Indigo2, both using discs from origami-dist.irix.fans. Both have installed over the network on an emulated Indy, and the 5.3 config also on an emulated R4000 Indigo.
- serve: `--json` writes status as JSON Lines on stdout for programs that run Instigator: `ready`, `bootp`, `tftp` with start, progress and end, `inst` sessions and returns, and a final `stopped` with its reason. The human log stays on stderr. The schema is in docs/json-output.md.
- serve: startup warns when `install_scripts` are configured for media from before IRIX 6.5, whose `inst` has no `admin source` to load them. Media count as pre-6.5 when the first set's distribution ships no `.related_dists`.
- serve: startup logs the R3000 Indigo's `stand/fx.IP12` alongside `fx.ARCS` and `fx.64`.
- qemunet: a write to the QEMU stream that stays blocked for five seconds logs a warning, and logs again with how long it blocked (or the error) when it returns. Before, a stalled `--network-tcp` install gave no sign of why.

Changed:
- serve: when the emulator closes the private network cleanly, `serve` and `share` exit 0. Before, they exited 1 with `private network disconnected: read frame: EOF`. A close in the middle of a frame or another link error still exits 1. A systemd unit for these modes needs `Restart=always` to restart after a disconnect.
- tftp: when a PROM ends a transfer after taking data, as it does reading the miniroot from `dist/sa`, the log says so at INFO and the capture records `client_stopped`. Before, each normal miniroot load logged a WARN and counted as an aborted transfer. An ERROR before any data still counts as aborted.

## 0.5.1 - 2026-10-30

Installs over a private QEMU network no longer stall for minutes after the guest drops packets.

Fixes:
- qemunet: installs no longer stall for minutes after lost packets. When an emulated machine's network card drops a burst, Instigator resends the rest straight away instead of waiting longer after every retry. Before, one install could sit idle for more than ten minutes in total.
- install shell: directory listings and other short output go out in fewer, fuller packets, so emulated machines drop them less often.

## 0.5.0 - 2026-10-30

Instigator can now share host folders with IRIX as well as serve installation media, and the release adds ready-made IRIX configurations and guides for installing, sharing, extracting files and troubleshooting.

File sharing: `instigator share` exports host directories read-write over NFSv2 and NFSv3 on UDP and TCP, with portmap and MOUNT. It listens on the host network or a private QEMU network, and private-network mode needs no root privileges. NFS has no NLM locking or NFSv4 support. The sharing guide covers permissions, ports and limitations.

Features:
- share: `serve --share NAME=DIR` adds shares alongside installation media on a private network. Repeat `--share` for several exports.
- share: shares stay inside their host directory, including when resolving symlinks. Guest writes run as the user who started Instigator. File handles survive restarts, with a persistent path table on Windows and on filesystems without file birth times. Path-table handles cannot detect a host process replacing a file at the same path.
- configs: shipped configs cover IRIX 6.5.30 standard and minimal installations, and IRIX 6.5.22 standard installations. Media URLs are filled in; change the server and client addresses before use. The general `addons:` list adds the supplied MIPSpro or RAD4 installation files.
- IP22: sets print the 32-bit `fx.ARCS` partitioner boot command. The hardware guide explains returning to the maintenance menu to boot the installer. Indy and Indigo2 installation is untested on hardware.
- DHCP: private networks answer DHCP alongside BOOTP and can offer a hostname. `share --client MAC=IP,HOSTNAME` supplies those settings without an installation config.
- DHCP: `dhcp_server_id` and `share --dhcp-server-id` let a guest keep the same DHCP server identity when moving between private and QEMU user networks. For Origami's default user network, use `192.0.2.2`. The server's own address and BOOTP replies are unchanged. Long-term lease renewal has not been observed from IRIX.

Fixes:
- source: remote image reads retry transient range-request failures.
- nfs: UDP socket buffers are raised for macOS transfers.
- share: private share shutdown no longer blocks on failure reports.

## 0.4.2 - 2026-10-29

NFS export and QEMU network changes.

Features:
- nfs: the read-only server can record RPC transcripts.

Fixes:
- nfsexport: symlinks are exported, and a read past the end of a file returns empty.
- qemunet: Ethernet datagrams are now compatible with the firmware.

## 0.4.1 - 2026-10-28

`check` probes remote sources instead of reading them.

Changed:
- check: remote sources are probed instead of read.

## 0.4.0 - 2026-10-19

Instigator can now extract files from SGI install media and run add-on installation and post-install commands from generated scripts. Source builds require Go 1.27.1 or newer.

Breaking:
- Source builds require Go 1.27.1 or newer. Migration: upgrade Go before building from source.

Features:
- extract: `instigator extract` reads products from images, directories or configured install sets and writes their files with a JSONL manifest. `--manifest-only` inspects entries without reading archives. `--unpack` removes installed `.Z` and `.z` compression. Extraction combines ordered sets into an installed view but does not apply every replacement rule that IRIX `inst` uses, so check the manifest before treating the output as an exact installed system.
- add-ons: mark third-party media `addon: true` and select it in an install script's `then` pass to install it after the operating system. This preserves operating-system links that a driver could otherwise replace with directories during the first pass. Each `go` is timed separately.
- post-install: an install script's `post_install` commands run through `shroot` after its last `go`. The completion line appears only after every command succeeds. On success Instigator restores the default `/bin/csh` shell preference. If a command fails, `inst` stops the command file and leaves `/bin/sh` selected.

Changed:
- Archive: release archives include the third-party notices for the extraction decoders.

## 0.3.2 - 2026-10-04

Instigator reports its executable version and records elapsed time for generated install attempts. Source builds require Go 1.27.1 or newer.

Breaking:
- Source builds require Go 1.27.1 or newer. Migration: upgrade Go before building from source.

Features:
- version: `instigator --version` reports the module version embedded by Go. Submodule and worktree builds record Instigator's own revision and dirty state.
- install scripts: generated scripts log timestamped starts and returns from `go`, including the script, client, attempt and elapsed time. Captures keep these events, and trace summaries pair them by attempt; missing returns stay incomplete. The timing includes marker fetch overhead and prompts during `go`, and a return confirms only that execution reached the command after `go`, so verify installation success from guest output and first boot. Handwritten scripts have no timing markers.

Changed:
- CLI: command-line failures include a timestamp and error severity.

Generated handoffs were tested with installed IRIX 6.5 `inst` 4.1. A complete successful miniroot installation with the timing markers has not been verified.

## 0.3.1 - 2026-10-03

Private-network installs use fewer backing reads, allocations and socket writes.

Changed:
- install shell: small `dd` blocks are grouped into about 32 KiB transfers, preserving count, skip, record accounting and foreground interruption.
- qemunet: private networking batches up to 32 frames already queued and flushes immediately when the queue empties. Receive scratch is reused after packets are copied into gVisor-owned storage.
- install shell: IRIX SIGINT requests interrupt supported foreground file transfers, so the installer can seek without first receiving the rest of each file. Background sessions and unsupported pipelines keep their previous signal behaviour.

Fixes:
- qemunet: a complete QEMU frame header with a missing body is reported as a truncated frame.

In one fresh-disk QEMU/IRIX comparison on macOS, the grouped-transfer and framing changes reduced the package phase from 27m45s to 22m51s, and median fixed-transfer throughput rose from 25.3 to 31.9 MB/s. All six guest checksums matched, and both installations rebooted to serial login. Both installers logged the same package diagnostics. The comparison predates the SIGINT change and does not measure its additional effect. Details and limits are in `docs/qemu-network-performance.md`.

## 0.3.0 - 2026-09-29

IRIX media validation, V7 archives, and Windows private-network installs.

Features:
- check: `instigator check` assembles and validates the configured media tree before service ports are bound. It accepts a config file or standard input.
- sources: local `.tar`, `.tardist` and compressed archive sources are supported. Ordered `replacements` entries let a named later layer replace files from one earlier layer. Unrelated conflicts still require an exact collision winner.
- archives: V7 tar archives inside gzip unpack through the bounded extraction path. Named install selections are checked against served `.sw` or `.man` files, including the selected package type.
- Windows: QEMU installs can use `--network-tcp` with an explicit loopback IP and port. It uses the same framed private network as the Unix socket mode, and the first local process to connect takes the single install session.
- install shell: private install shells stay open during long quiet transfers. If the QEMU machine disconnects, Instigator stops serving and records that reason in the capture.

Changed:
- inst: when `inst` opens its numbered distribution menu after the primary source is reopened, choose the `done` item on the guest console. The sourced install script then continues with its package selections.

## 0.2.1 - 2026-09-05

Readiness signalling, always-lazy raw images, and multi-homed BOOTP.

Features:
- serve: reports ready once the install sets are built and the listeners are bound, so a `Type=notify` unit or `systemd-notify --fork` learns when it is serving.

Changed:
- source: raw images are always read by HTTP byte-range, so a full disc is never downloaded just to verify it. The per-layer `sha256` key is gone, and ignored if still present.
- serve: the operational log moves to stderr, and a readiness report that cannot be delivered is now fatal instead of ignored.
- CLI: the command line is parsed with `urfave/cli`.

Fixes:
- bootp: replies leave by the interface the request arrived on, which is correct on a multi-homed host.

## 0.2.0 - 2026-09-02

Layers can come from local or remote sources, with a cache, credentials and byte-range reads, and releases now include macOS and Windows binaries.

Features:
- source: a local or remote source resolves to a read-only filesystem, with safe extraction of raw, gzip and tar archives. Remote reads use a chunk-caching byte-range reader with a consistency pin, fetch timeouts and a content-identity cache with atomic install. Whole-file fetches can verify a sha256.
- config: layers take `source`, `base`, `stand` and `sha256` keys, and a `credentials` block gives host-matched basic auth. Credentials are stripped on a cross-host redirect.
- vfs: install sets are built from a resolved source, with base and stand, through one `io/fs.FS` walker. A shared source is verified against every layer's digest.
- efs: a read-only `io/fs.FS` view over an image carries inode modes and reports special-file types.
- serve: setuid, setgid and sticky bits on served files are kept, and URL sources are redacted in logs and `run.json`. A refused bind hints at privilege.
- release: builds macOS and Windows binaries.

Fixes:
- bootp: `SO_BROADCAST` is enabled on Windows.
- rcmd: Winsock error codes match on Windows.
- efs: a missing entry is told apart from a read error, and directory entries are sorted.
- source: a 200 reply to a ranged chunk request is rejected instead of cached, EOF is signalled only on a short range read, the cache is keyed on the full URL, and concurrent reads are guarded. The tar extractor's symlink checks and the credential redirect check are hardened.
