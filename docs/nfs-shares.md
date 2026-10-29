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
- No locking (NLM) and no NFSv4. IRIX ignores `nolock`; mounting with
  `private` keeps locking local, and plain copies in and out work
  without it.

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

Each running machine gets an in-process network stack, a host of its own
on the machine's network that serves portmap, MOUNT and NFS, and a small
Ethernet switch that puts it beside whatever network the machine uses.

IRIX's `mount` always asks portmap, over UDP port 111 of the server
address, before it contacts MOUNT or NFS, whatever `port=` says. QEMU's
`guestfwd` is TCP-only, so it can't carry that call. The server has to
be a host on the guest's network, answering on UDP 111 itself.

The wiring uses a hub and two QEMU stream netdevs. The machine's own
network, whatever its mode, joins hub 1; the guest NIC is a stream
client of the switch instead:

```
-netdev user,id=net0,net=192.0.2.0/24,host=192.0.2.2,dhcpstart=192.0.2.15,...
-netdev hubport,id=hu,hubid=1,netdev=net0
-netdev stream,id=sB,addr.type=inet,addr.host=127.0.0.1,addr.port=PB,server=off,reconnect-ms=1000
-netdev hubport,id=hsb,hubid=1,netdev=sB
-netdev stream,id=sA,addr.type=inet,addr.host=127.0.0.1,addr.port=PA,server=off,reconnect-ms=1000
-net nic,model=<NIC>,netdev=sA
```

Stream A carries the guest's frames and stream B everything on the
machine's network. QEMU frames each Ethernet frame with a 4-byte
big-endian length. The switch knows nothing of what is upstream:

- From the guest: a frame for the stack's MAC or IP goes to the stack
  only. A broadcast goes to the stack and to the network. Everything
  else goes to the network.
- From the network and from the stack: to the guest only.

Without the switch, a hub alone floods every frame to the upstream
network as well as the stack, which answers for frames meant for the
stack and races its replies.

The stack takes a spare address on the network, with a fixed MAC in the
08:00:69 prefix. It answers ARP and ICMP echo and serves portmap, MOUNT
and NFS on UDP and TCP ports 111, 635 and 2049. The streams reconnect
every second, so a restarted `origami run` picks the guest back up.

| Mode | `net0` behind the hub | Stack address |
|---|---|---|
| user | slirp | 192.0.2.4 (slirp is 192.0.2.2) |
| private | the install network's stream client | 10.98.0.4 (Instigator is 10.98.0.2) |
| tap, vmnet | the tap device or socket_vmnet | set by the user, with its prefix length |

Facts the spikes established against IRIX 6.5.30:

- The mount is `mount ADDRESS:/NAME /mnt/x`. IRIX negotiates NFSv3 over
  TCP; `-o vers=2,proto=udp` also works, as do v2/v3 over either
  transport.
- `nolock` is ignored on IRIX. `private` is the option that makes
  locking local.
- READDIR doesn't list `.` or `..`, and IRIX accepts that.
- A read-only export fails every write immediately with `ROFS`.

## CLI

```sh
origami share my-octane src ~/code          # saved in machine.toml
origami share my-octane src --remove
```

IRIX side, printed by `origami share` and given once in the guide:

```sh
mkdir -p /mnt/src
mount 192.0.2.4:/src /mnt/src
```

The address is the stack's on the machine's network; `origami share`
prints the line for the machine's current mode.

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

1. **Spike.** Instigator's v2/UDP server, read-only, mounted from a
   6.5.30 guest. It found that `mount` always asks portmap over UDP, which
   led to the stack-and-switch wiring in "Networking".
2. **Instigator: transcripts.** The transcript harness and the fixture,
   recorded from the current v2 read-only server.
3. **Instigator: v3 and TCP,** read-only, with transcripts.
4. **Instigator: writes** for v2 and v3, with the owner and handle rules
   above and their transcripts.
5. **Origami: Rust crate** (`origami-nfs`) ported from the Go server,
   passing every transcript.
6. **Origami: userspace stack and switch,** then the `share` command and
   the guide, plus the IRIX check against both servers.

Each step is its own PR with its tests. Steps 2–4 can land and release
in Instigator before the port starts.

## Open questions

- Does the userspace switch slow normal guest networking? The IRIX check
  compares an internet download through it with plain user mode.
