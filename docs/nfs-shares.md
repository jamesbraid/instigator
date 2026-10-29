# Host folder shares over NFS

## Goal

`origami share` makes a host directory appear inside an IRIX guest,
read-write, through NFS. That needs no root, no host NFS server and no
privileged port. We own both server implementations:

- Instigator (Go) keeps its NFS package as the reference server.
- Origami (Rust) embeds a port of it and runs it for each running machine.

Install media stays on Instigator's HTTP and rcmd paths. NFS here is for
shares only.

## What exists

Instigator's `nfs` package, BSD-3-Clause, owned by us:

- NFSv2 and NFSv3 over UDP and TCP, with portmap and MOUNT beside them.
  Every listener answers all three programs, so a client that knows the
  port needs no portmap call.
- An `FS` interface that `internal/nfsexport` adapts to any `io/fs` tree,
  served read-only: every write returns `ROFS`.
- A `WriteFS` extension that `internal/sharefs` implements for a host
  directory, under the rules in "Files, owners and handles" below.
- `instigator share` serves one host directory, apart from the install
  path.

IRIX 6.5.30's client mounts NFSv3 over TCP and NFSv2 over UDP. The
development tooling's `--share` (unfsd, NFSv3/TCP) already mounts and
writes from a 6.5.30 guest, and the installs now include `nfs.sw.nfs`.

## Protocol coverage

Both servers implement the same set:

| Program | Versions | Procedures |
|---|---|---|
| portmap (100000) | 2 | NULL, GETPORT, DUMP |
| MOUNT (100005) | 1, 3 | NULL, MNT, UMNT, UMNTALL, EXPORT |
| NFS (100003) | 2 | every RFC 1094 procedure |
| NFS (100003) | 3 | every RFC 1813 procedure except LINK, which returns `NFS3ERR_NOTSUPP` |

Transports: UDP and TCP with record marking, for every program.

- Writes are synchronous to the host file. WRITE replies `FILE_SYNC`, and
  COMMIT is a no-op that returns the server's boot verifier.
- No locking (NLM) and no NFSv4. IRIX mounts with `nolock`, or local
  locking, and that is enough for copying files in and out.

## Files, owners and handles

- **One export per share.** It is rooted at the host directory, and the
  server never resolves a path outside it, including through symlinks.
- **Owners.** Every file the guest sees is owned by uid 0 and gid 0, so
  root in the guest can do anything. Every host operation runs as the
  user who started Origami, so files land owned by that user. Guest chown
  and chgrp succeed without changing the host file. Mode bits pass through
  from the host, except the setuid bit.
- **Handles.** They carry the share id, the host inode and device, and a
  generation from the file's birth time where the host has one. A handle
  that no longer matches the file returns `STALE`. Handles survive a
  server restart, because they are derived from the file, not from server
  memory.
- **Windows.** The host has no inode, so handles map the path through a
  persistent table in the machine directory. Windows gets the same
  owner rules.

The handle is 32 bytes: a format byte (2 for inodes, 3 for a path
table), three zero bytes, the share id (FNV-1a 32 of the share name),
then the device, inode and generation as 64-bit big-endian numbers. With
a path table, the inode is the table id and the other two are zero. On
Linux the generation is the birth time from `statx` in nanoseconds, on
macOS the birth time `stat` reports, elsewhere zero. A handle the cache
doesn't place, such as one from before a restart, is found by walking
the share for its device and inode.

The path table is a text file of `set ID PATH` lines, with `PATH` quoted
as Go quotes strings, and `del ID` lines. Ids count up from 1 in the
order paths are first seen, and a rename keeps them. A path is first
seen when the server first stats it: the root at MNT, a new file when it
is made, and a directory's entries in name order when it is listed.

A READDIR cookie for a share entry is FNV-1a 32 of its name with the top
bit cleared, or 1 if that is 0, and entries are listed in cookie order,
then by name. A listing resumes after the cookie the client last saw, so
removing entries already listed, as `rm -r` does, skips nothing. The
read-only tree numbers entries by position instead. Neither lists `.`
or `..`. `LOOKUP ".."` of a share's root is the root.

A UDP reply to a call that changes the share, other than WRITE, is kept
and resent for an identical retransmission, so a REMOVE retried after a
lost reply doesn't come back NOENT.

## Networking

Each running machine gets its own server inside the `origami run`
process, listening on loopback ports Origami chooses.

- **User mode.** The guest reaches the host as 192.0.2.2. QEMU's
  `guestfwd` forwards guest TCP to 192.0.2.2:111 and :2049 into the
  server's loopback ports. `guestfwd` is TCP-only, so shares in user mode
  use NFSv3 over TCP. The guest-side mount names the ports explicitly.
- **Private and tap modes.** The server binds the host-side address of
  that network on 111 and 2049. When it can't, because the ports are
  privileged or taken, it binds high ports and the mount names them.
- **Mount command.** IRIX must be able to mount with explicit `port=`
  options and no portmap call. That is the first thing the spike settles.

## CLI

```sh
origami share my-octane src ~/code          # saved in machine.toml
origami share my-octane src --remove
```

IRIX side, printed by `origami share` and given once in the guide:

```sh
mkdir -p /mnt/src
mount -o vers=3,proto=tcp,port=2049,nolock 192.0.2.2:/src /mnt/src
```

`origami show` lists shares. Adding one to a running machine takes
effect at the next `run`.

## Keeping Go and Rust identical

- **Shared transcripts.** Every procedure, error case and transport is a
  file of RPC request bytes paired with the expected reply bytes, run
  against a fixed fixture tree. The fixture and transcripts live in
  Instigator. Origami's crate test-imports them through the existing
  submodule.
- **A change to either server** lands with a transcript, and CI in both
  repos replays every transcript.
- **The IRIX check** runs against each server before a release that
  touches it: mount, list, read, write, rename, remove, mkdir, a 3 GB file,
  unmount and remount. It covers NFSv3/TCP, plus NFSv2/UDP on a private
  network.

The Rust port is the `origami-nfs` crate, at `crates/origami-nfs` in the
sgi-emulator repository (the Origami product CLI) and a member of its
Cargo workspace. Its test `tests/transcripts.rs` reads `nfs/testdata` from
the `instigator` submodule and replays every transcript over UDP and TCP,
requiring identical bytes. The CLI tests workflow runs it with
`cargo test --workspace`, so moving the submodule pin to a newer Instigator
replays that Instigator's transcripts. A Windows host skips the share
transcripts, which need Unix modes and symlinks.

## Transcript format

The transcripts are `nfs/testdata/transcripts/*.txt`, plain text read
line by line. Blank lines and lines starting with `#` are comments.

```
fixture tree

call GETATTR the root
> 00000003 00000000 00000002 000186a3 00000002 00000001 00000000 00000000
> 00000000 00000000 01000000 00000000 01000000 00000000 00000000 00000000
> 00000000 00000000
< 00000003 00000001 00000000 00000000 00000000 00000000 00000000 00000002
```

- `fixture NAME` names the tree the server exports, both built from
  `nfs/testdata/fixture.txt`, whose header comment gives its format.
  - `tree` serves it read-only, as `/`, numbered as that comment says.
  - `share` writes it to a temporary directory, with the listed modes and
    modification times, and serves that as the share `/share` with a path
    table, so handles and fileids follow the path table's numbering. The
    server's write verifier is the eight bytes `verifier`. Attributes
    the host decides are pinned before they are encoded: any
    modification time after 2010-01-01 (1262304000), and every change
    time, becomes 2000000000 seconds; the access time is shown as the
    modification time; a directory has size 4096 and two links; and
    storage used is the size rounded up to 4096. A SETATTR asking for
    server time is sent as the `SET_TO_SERVER_TIME` it is.
- `call LABEL` starts an exchange. The label is for people.
- `>` lines carry the RPC call message and `<` lines the reply, as hex.
  Whitespace inside the hex is only for reading; the bytes concatenate.
  The message is the RPC message alone (RFC 5531 section 9), with no
  record mark: a TCP replay adds one and expects one.
- Exchanges run in order against one fresh server and fixture, and the
  reply must match exactly. A replay sends every exchange to one UDP
  socket, then again on one TCP connection to a second fresh server.
  Portmap advertises itself on port 111, MOUNT on 635 and NFS on 2049 for
  both transports, whatever ports the test server is really bound to.

The Go scenarios in `nfs/transcripts_scenarios_test.go` generate the
files. `go test ./nfs -run TestTranscripts -update` rewrites them from
the current server, and a plain `go test` fails when a scenario no longer
produces its file byte for byte, then replays each file's raw calls.

## Order of work

1. **Spike.** Instigator's v2/UDP server, read-only, behind `guestfwd`
   on a private network. Mount it from a 6.5.30 guest with explicit
   ports. This settles the mount options IRIX accepts with no portmap.
2. **Instigator: transcripts.** The transcript harness and the fixture,
   recorded from the current v2 read-only server.
3. **Instigator: v3 and TCP,** read-only, with transcripts.
4. **Instigator: writes** for v2 and v3, with the owner and handle rules
   above and their transcripts.
5. **Origami: Rust crate** (`origami-nfs`) ported from the Go server,
   passing every transcript.
6. **Origami: `share` command,** networking and the guide, plus the
   IRIX check against both servers.

Each step is its own PR with its tests. Steps 2–4 can land and release
in Instigator before the port starts.

## Open questions

- Does IRIX 6.5.30's `mount` accept `port=` and a MOUNT-protocol port
  without asking portmap? If it can't, user mode needs a portmap that
  answers over TCP through `guestfwd`. The spike decides.
- Does QEMU's `guestfwd` keep up with sustained NFS traffic? The 3 GB copy
  in the IRIX check measures it.
