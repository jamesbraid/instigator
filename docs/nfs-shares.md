# NFS protocol and Origami integration

For commands to share a host folder with IRIX, start with the
[sharing guide](sharing.md). This reference covers protocol behavior, file
handles, private networking, and the transcripts shared by the Go server
and Origami's Rust port.

Instigator's `nfs` package implements NFSv2 and NFSv3 over UDP and TCP,
with portmap and MOUNT. `internal/sharefs` adapts host directories for
writable shares; `internal/nfsexport` adapts `io/fs` trees for read-only
exports. The `share` command serves host folders on its own, and
`serve --share` adds them to the private install network. Install media
continues to use BOOTP, TFTP, and rsh.

The Rust port is embedded in Origami. Sections describing `origami share`
and its network switch concern that integration, rather than Instigator's
standalone command.

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
  user who started the server, so files land owned by that user. Guest chown
  and chgrp succeed without changing the host file. Mode bits pass through
  from the host, except the setuid bit.
- **Handles.** They carry the share id, the host inode and device, and a
  generation from the file's birth time where the host has one. A handle
  that no longer matches the file returns `STALE`. Handles survive a
  server restart, because they are derived from the file, not from server
  memory.
- **No birth time.** A filesystem without birth times, such as an NFS
  mount, hands a freed inode to the next file created, so an inode and a
  zero generation cannot tell a recreated name from the file it replaced.
  A share opened on one uses path-table handles instead, the same as
  Windows, in a file in the user cache directory named for the share and
  its directory (`--handle-table` picks another). The server probes the
  share's root once, at start.
- **Windows.** The host has no inode, so handles map the path through a
  persistent path table. Windows gets the same
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
as Go quotes strings, `del ID` lines, and a `next ID` line at the start of
a compacted table. Ids count up from 1 in the order paths are first seen,
and a rename keeps them. An id is never handed out twice: a name removed
or renamed over through the server loses its id, and the file made under
that name next gets a new one, even after any number of restarts. A path is first
seen when the server first stats it: the root at MNT, a new file when it
is made, and a directory's entries in name order when it is listed.

What path-table handles do not cover: a file the host deletes or replaces
behind the server's back, with the server running or stopped, keeps its
path and so its handle, and the new file answers to it. Only changes made
through NFS retire an id. Inode handles with a birth time detect that, and
a host with one uses them.

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

## Private network

The private network is the Ethernet segment Instigator builds for one QEMU
machine, with no host privileges: a user-space IPv4 stack (gVisor's
netstack, in `internal/qemunet`) attached to the machine's NIC over a byte
stream. `serve` runs the install services on it, `serve --share` and
`share --network-socket` or `--network-tcp` add portmap, MOUNT and NFS,
and the Rust port in Origami builds the same segment. This section is
what the two must agree on. The frame transcripts under
`internal/qemunet/testdata/transcripts` pin the parts a test can pin; the
rest is stated here.

### The link

- **Attachment.** Instigator listens on a loopback TCP address
  (`--network-tcp IP:PORT`; a non-loopback address or port 0 is refused)
  or a Unix socket (`--network-socket PATH`) and accepts the machine's
  one connection. QEMU is the client:
  `-netdev stream,id=net0,server=off,addr.type=inet,addr.host=127.0.0.1,addr.port=PORT,reconnect-ms=1000`
  with `-net nic,model=sgi-ioc3-eth,netdev=net0`. A restarted Instigator
  picks the machine back up, because QEMU retries every second.
- **Stream framing.** Each Ethernet frame is a 4-byte big-endian length,
  then that many bytes: the frame from the destination MAC through the
  payload, with no preamble and no FCS. A length over 65535 is a protocol
  error and closes the stream. The two directions are independent. A
  stream that ends between frames is a disconnect; one that ends inside a
  frame is a truncated read. Either ends the network, and `serve` and
  `share` exit with `private network disconnected`. They do not wait for a
  second connection.
- **Padding.** Frames the host sends are padded with zeros to 60 bytes,
  the Ethernet minimum without FCS. Longer frames are sent as built.
- **MTU.** 1500. IPv4 only: IPv6, and any ethertype but ARP (0x0806) and
  IPv4 (0x0800), is dropped without a reply.

### Addresses

| | |
|---|---|
| Subnet | 10.98.0.0/24 |
| Host (Instigator) | 10.98.0.2, MAC 08:00:69:00:00:02 |
| Machine | 10.98.0.65 in Origami's install configuration; the address is the `ip` of the client entry whose `mac` is the machine's |
| Machine MAC | the machine's own (an IRIX machine's is in the 08:00:69 prefix); it is the `mac` of the client entry |
| Gateway, DNS | none: the segment has no router and no name service |

The host's address and prefix come from the configuration (`server_ip`,
`netmask`) or `share --address ADDR/PREFIX`; the MAC is fixed. The
defaults above are what Origami uses.

### ARP and ICMP

- **ARP.** The host answers a request whose target is its own address
  with a reply carrying its MAC, whatever the sender address is, a
  `0.0.0.0` probe included. It answers no other target and sends no
  announcement, on connecting or otherwise. A reply goes to the MAC of the frame it
  answers. When the host must send first, as the rsh callback does, it
  broadcasts a who-has with its own address as sender and waits.
- **ICMP.** An echo request to the host address is answered with an echo
  reply that copies the identifier, sequence number and data; so is one to
  the subnet broadcast address 10.98.0.255 (sent from the host address).
  An echo to any other address draws nothing. A UDP datagram to a port
  nobody serves draws an ICMP destination unreachable, port unreachable,
  quoting the offending datagram; one to a broadcast address draws
  nothing.
- **IPv4 header.** Replies the host originates carry TTL 64 and no DF. The
  identification field is random, so the transcripts compare replies with it
  set to zero and the header checksum recomputed.

### UDP, TCP and fragmentation

UDP datagrams up to 65507 bytes are sent and received; the host
fragments at the 1500-byte MTU, sets no DF, and reassembles what the
machine fragments. NFSv2 over UDP with 8 KiB READs depends on this. TCP is
ordinary TCP over that MTU; the transcripts do not pin sequence numbers
or segment boundaries.

| Service | Transport and port | When |
|---|---|---|
| BOOTP | UDP 67 | `serve` with `services.bootp` |
| TFTP | UDP 69, then a port per transfer | `serve` with `services.tftp` |
| rsh | TCP 514; the host dials back to the machine's stderr port from a source port in 512 to 1023 | `serve` with `services.rsh` |
| portmap, MOUNT, NFS | UDP and TCP 111, 635, 2049 | `--share` given |

Every one of the three NFS ports answers all three programs, and portmap
advertises 111, 635 and 2049 for both transports. With no `--share`
nothing listens on them: UDP to 111 draws the port unreachable above.

### Addressing the machine: BOOTP, DHCP and the guest

- **BOOTP.** Instigator answers legacy BOOTP (RFC 951) for the machines
  named in its configuration, and for nobody else. A request is a UDP
  datagram to port 67 of at least 300 bytes with op 1, hardware type 1 and
  length 6; shorter ones are ignored. The reply is 300 bytes: op 2, the
  machine's configured address in `yiaddr`, the host's address in
  `siaddr`, `sname` cleared, the request's `file` echoed, and a vendor area
  of the RFC 1048 cookie (99 130 83 99), option 1 subnet mask when the
  configuration has a netmask, option 12 host name when the client has one,
  and the end option. Anything else in the request is copied back. The
  reply goes to the request's source port, not port 68: broadcast
  (255.255.255.255, MAC ff:ff:ff:ff:ff:ff) when the request's source
  address is `0.0.0.0`, otherwise unicast to that source address.
- **Host name.** A client entry's optional `hostname` (letters, digits,
  hyphens and dots, 40 bytes at most) is sent as option 12. Without it the
  option is absent and the reply is byte for byte what it was before the
  option existed. Origami's `install-serve` writes the client as
  `{"name": "sgi", "mac": ..., "ip": "10.98.0.65"}` and does not set it;
  it must pass the machine directory's name as `hostname` for BOOTP to
  carry the name. Whether the PROM and the miniroot take option 12 from a
  BOOTP reply has not been run against IRIX; the installed system takes it
  from DHCP, below.
- **DHCP.** On the private network the same service answers DHCP
  (RFC 2131) for the same clients, by MAC, and for nobody else. Off the
  private network, on a host's own interface, a message carrying option 53
  is still ignored, so a DHCP service on that LAN keeps port 67.
  - A DISCOVER is answered with an OFFER, and a REQUEST with an ACK. The
    address is always the client entry's `ip`. A REQUEST for another
    address draws a NAK, whether it names the host's server identifier
    (selecting) or has none (init-reboot), and a REQUEST that carries
    another server's identifier draws nothing, as do DECLINE, RELEASE and
    INFORM. A renewing REQUEST, with `ciaddr` set and no option 50, is
    answered like the others by comparing `ciaddr`.
  - The server identifier is the host's address unless the configuration
    sets `dhcp_server_id` (`share --dhcp-server-id`). It goes in option 54
    of every reply and is the one a selecting REQUEST must name. Origami
    sets it to 192.0.2.2, QEMU's user network's DHCP server; see "Moving
    between networks" below.
  - Replies are 300 bytes at least. `yiaddr` is the machine's address,
    `siaddr` the host's and option 54 the server identifier, `chaddr`, `xid` and `flags` are the
    request's, and `ciaddr` is the request's in an ACK and zero otherwise.
    A NAK has neither `yiaddr` nor `siaddr`. Options, in order: 53, 54,
    then in an OFFER or ACK 51 (lease), 1 (subnet mask, when the
    configuration has a netmask) and 12 (host name, when the client entry
    has one), then 255. There is no router (option 3), DNS or anything
    else.
  - The lease is 31536000 seconds, a year, fixed: IRIX asks for exactly
    that. Nothing in a reply is random or depends on the time, so the same
    request always draws the same bytes.
  - Replies go to UDP port 68 and are broadcast (255.255.255.255, MAC
    ff:ff:ff:ff:ff:ff), because the machine has no address to unicast to
    and the host does not ARP for it. The one exception is an ACK to a
    renewing REQUEST, which goes to `ciaddr`.
  - BOOTP is unchanged and still answered beside DHCP: a request without
    option 53 is BOOTP, with the 300-byte rule above.
- **Share-only mode.** `instigator share --network-socket|--network-tcp`
  has no configuration and so no client list. `--client MAC=IP[,HOSTNAME]`
  (repeatable, private network only) names the machines it answers, and
  with it the host answers BOOTP and DHCP exactly as `serve` does, from
  the host address and `--address` prefix as the subnet mask. The IP may be
  left out for a single client and is then 10.98.0.65 (`MAC` alone, or
  `MAC=,HOSTNAME`). It must lie in the segment and not be the host's own,
  and no two clients share a MAC or an IP. With several clients each
  needs an IP. Without `--client` nothing listens on 67, as before. The
  hostname rules are the client entry's. `--dhcp-server-id ADDR` sets the
  server identifier, as `dhcp_server_id` does.
- **How the installed guest gets an address.** IRIX's DHCP client
  (`proclaim`) starts at boot when `autoconfig_ipaddress` is on, which
  Origami's install post-commands set. It broadcasts a 548-byte DISCOVER
  from `0.0.0.0:68` with the broadcast flag clear, naming the host name
  `IRIS` (option 12) and asking for lease 31536000 (option 51), receives
  the OFFER and sends a REQUEST for it. `ef0` comes up with the address,
  the mask and the route to the segment with nothing configured by hand.
- **Moving between networks.** proclaim keeps its lease in
  `/var/adm/proclaim.lease_info_ascii.ef0` (and a binary
  `proclaim.lease_info.ef0`), and at boot sends an init-reboot REQUEST
  for the address `/etc/hosts` gives the host name, with no server
  identifier. On the same network the host acknowledges it. A machine
  that last booted on QEMU's user network asks for 192.0.2.15, and the
  host answers with a NAK as RFC 2131 4.3.2 requires. proclaim then
  discovers again, but rejects every OFFER whose server identifier is not
  the one in its lease file ("Server mismatch to the DHCPREQUEST
  broadcast" in `/var/adm/SYSLOG`), 193 times in under a second, until it
  runs out of file descriptors and exits. `ef0` keeps the old address, and
  nothing on 10.98.0.0/24 is reachable. QEMU's user network gets the same
  loop in the other direction. With `dhcp_server_id` set to the user
  network's 192.0.2.2 both networks name one server, the OFFER matches
  the lease, and proclaim takes it and rewrites `/etc/hosts`. An ACK from
  a different identifier, for the address the machine already has, is
  only logged as a mismatch and accepted, so a lease from 10.98.0.2 moves
  to the new identifier on the next boot. A client following RFC 2131
  renews by unicast to the identifier at half the lease, which from this
  segment goes nowhere, then rebinds by broadcast at 87.5%, which the host
  answers by `ciaddr`. With a year's lease neither has been seen from IRIX.
- **Host name.** IRIX uses it. The REQUEST that follows an OFFER carrying
  option 12 asks for that name, and the system sets its host name and
  `/etc/sys_id` to it: a machine whose client entry has `hostname:
  origin200` boots as `origin200`. Without the option the machine keeps
  the name it has (`IRIS` on a fresh install).

### Transcripts

`internal/qemunet/testdata/transcripts/*.txt` hold one segment each,
configured by their header, and replay the same way as the RPC transcripts:

```
server 10.98.0.2/24 08:00:69:00:00:02
bootp 08:00:69:12:34:56 10.98.0.65 hostname=origin200

frame request from the configured machine
> ffffffff ffff0800 69123456 08004500 01480001 00004011 79a50000 0000ffff
< ffffffff ffff0800 69000002 08004500 01480000 00004011 6f420a62 0002ffff
frame an address nobody owns
> ...
none
```

- `server ADDR/PREFIX MAC` is the host and must be the one above. The
  optional `bootp MAC IP [hostname=NAME] [dhcp]` starts the BOOTP service
  with that one client, and `dhcp` makes it answer DHCP too; without the
  line nothing listens on 67.
- `frame LABEL` starts an exchange. `>` lines are the Ethernet frame the
  machine sends and `<` lines the frame the host answers, as hex, with no
  length prefix: a replay adds the 4-byte length on the way in and removes
  it on the way out. `none` means the host sends nothing, which a replay
  establishes by waiting 400 ms.
- Exchanges run in order on one fresh segment. A reply is compared after
  its IPv4 identification is zeroed and the header checksum recomputed.
- An exchange has at most one reply frame. A scenario that makes the host
  send more, or ask on its own (ARP for an address it must resolve), is
  not in the files, because the extra frames' timing is the netstack's.

`go test ./internal/qemunet -run TestFrameTranscripts -update` rewrites the
files from the stack. The files cover ARP, ICMP echo and port unreachable,
BOOTP and DHCP with and without a host name, DHCP refusals, and ignored
traffic. The DHCP files include the DISCOVER and REQUEST IRIX's `proclaim`
sent, `dhcp-init-reboot.txt` the init-reboot, rediscovery and
reselection it sent after moving from QEMU's user network, and
`dhcp-server-id.txt` the same move with the identifier set, with the
machine's MAC replaced. The `bootp` line takes `server-id=ADDR` for
`dhcp_server_id`. The NFS programs have the RPC transcripts in `nfs/testdata`.

### Checked against IRIX 6.5.30

Origin 200 machine, private mode, Origami 0.4.1, `serve --share` and
`share --network-tcp`:

- The guest pings the host, `showmount -e` lists every share, and
  `rpcinfo -p` shows portmap, MOUNT v1 and v3, and NFS v2 and v3 on UDP and
  TCP at 111, 635 and 2049.
- `mount -o vers=3,proto=tcp` and `mount -o vers=2,proto=udp` both work, at
  once, from two shares on one server. ls, cat, write, append, mv,
  mkdir, symlink, chmod, rm and rmdir pass on both.
- A fresh boot of an installed 6.5.30 system with `autoconfig_ipaddress` on
  gets 10.98.0.65 from the DHCP service, with the mask and the segment
  route, and pings the host with no `ifconfig` by hand. With a `hostname`
  on the client entry it comes up under that name.
- A 200 MB file copies from each share to the guest's disk and back with
  identical SHA-256 on the host.
- A host filesystem with no birth time (an NFS-mounted directory, for one)
  recycles inode numbers, so a name that is removed and made again could get
  its old handle, and the guest reported `Stale NFS file handle`. Shares on
  such a filesystem now use path-table handles (see "No birth time"). With
  shares on an NFS mount (`/storage/nvme` here), `touch t; rm t; touch t;
  cat t` and the same with a two-second pause, three times on each mount,
  pass on both v2/UDP and v3/TCP, and so do the file operations above.

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
- **Frame transcripts.** The private network's frames have their own
  transcripts, under `internal/qemunet/testdata/transcripts`, read the
  same way through the submodule. See "Private network".
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

## Network performance to check

Compare an internet download through Origami's userspace switch with plain
QEMU user networking before claiming the switch has no throughput cost.
