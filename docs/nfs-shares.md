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

Instigator's `nfs` package, about 620 lines, BSD-3-Clause, owned by us:

- NFSv2 over UDP, with portmap and MOUNT beside it.
- Read-only: every write returns `NFSERR_ROFS`.
- 32-byte filehandles carrying a stable node id.
- An `FS` interface that `internal/nfsexport` adapts to any `io/fs` tree.
- Tested directly, but not linked into the `instigator` binary.

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
