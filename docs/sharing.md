# Sharing files with IRIX

`instigator share` exports host directories over NFSv2 and NFSv3, using UDP or
TCP. Use it to copy files into IRIX, get files out, or work on files from both
systems. It runs its own NFS server. You don't need to configure the host's
NFS service or install IRIX media first.

Shares are writable. Guest writes affect the host files, and the server uses
the permissions of the user who started it. Every file appears owned by root
in the guest. Guest `chown` and `chgrp` leave host ownership unchanged.

## On a physical network

Create or choose a host directory, then bind to the host address IRIX can
reach. Replace `192.0.2.10` with that address:

```sh
mkdir -p "$HOME/sgi-share"
sudo ./instigator share --listen 192.0.2.10 files "$HOME/sgi-share"
```

This exports the directory as `/files`. `sudo` is needed for the default
portmap port, 111, on Linux and macOS. Files created by this process are owned
by root on the host. On Windows, use `instigator.exe` from a terminal with
permission to bind the ports.

On IRIX, as root:

```sh
mkdir -p /mnt/files
mount 192.0.2.10:/files /mnt/files
ls /mnt/files
```

Leave Instigator running while the share is mounted. Unmount it before
stopping the server with Ctrl-C:

```sh
umount /mnt/files
```

Allow UDP and TCP ports 111 and 2049 through the host firewall. On host
sockets, NFS and MOUNT share port 2049. An existing host portmap or NFS server
can prevent Instigator from binding those ports.

The default `--listen` address is `127.0.0.1`, which a physical SGI cannot
reach. Set it to the host's address on the SGI's network. The server has no
per-client access list for NFS shares. Bind it on a trusted network or
restrict access with your firewall.

## On a private emulator network

Instigator can serve the share on a QEMU Ethernet stream instead of binding
NFS ports on the host. This mode needs no `sudo`, so host files are created as
your own user. For example:

```sh
mkdir -p "$HOME/sgi-share"
./instigator share --network-tcp 127.0.0.1:45031 \
  --client 08:00:69:12:34:56=10.98.0.65,iris \
  files "$HOME/sgi-share"
```

Replace the MAC with your guest's MAC. Configure the emulator to connect its
NIC to the stream at `127.0.0.1:45031`. Instigator listens. QEMU connects. The
[private network reference](nfs-shares.md#the-link) gives the QEMU arguments
and framing details. `--network-socket PATH` uses a Unix socket instead.
Choose one transport.

The server address defaults to `10.98.0.2/24`. `--client` makes it answer
BOOTP and DHCP for that MAC, offering `10.98.0.65` and the hostname `iris`.
For installed IRIX to request an address at boot, its `autoconfig_ipaddress`
setting must be on. Without `--client`, configure the guest's address
yourself. The private segment provides no gateway or DNS.

On IRIX:

```sh
mkdir -p /mnt/files
mount 10.98.0.2:/files /mnt/files
```

Private-network NFS uses UDP and TCP ports 111, 635, and 2049 inside the
segment. `--listen`, `--port`, `--portmap-port`, `--udp`, and `--tcp` do not
apply in this mode. The server exits when the emulator disconnects.

## Moving between networks

IRIX's DHCP client, `proclaim`, remembers the server identifier in its lease.
After moving to another network it can reject offers from a different
identifier and keep its old address. Give the private and user networks the
same identifier when they serve the same guest.

For Origami's default QEMU user network, add `--dhcp-server-id 192.0.2.2` to
the private `share` command. It requires `--client`. With `serve`, set
`dhcp_server_id: 192.0.2.2` in the configuration. Use your user network's
identifier if it differs from that default.

The private server still uses `10.98.0.2` unless you change its address.
The option changes only the DHCP identifier. When an existing lease already
holds the offered address, IRIX accepts the ACK and records the new
identifier for the next boot. Renewal to an identifier outside the private
segment cannot reach it, but broadcast rebinding can. Renewal over the
one-year lease has not been observed from IRIX. See the
[protocol reference](nfs-shares.md#addressing-the-machine-bootp-dhcp-and-the-guest).

## Shares alongside an install

Add one or more `--share NAME=DIR` arguments to `serve` on a private network:

```sh
./instigator serve --network-tcp 127.0.0.1:45031 \
  --share "files=$HOME/sgi-share" instigator.yaml
```

The server address and client list come from `instigator.yaml`. Shares are
optional and separate from install media. `serve` needs `--network-tcp` or
`--network-socket` to use them. For sharing on host sockets, use `share`.

## Multiple shares and server options

Use repeatable `--share NAME=DIR` arguments instead of, or alongside, the
positional name and directory:

```sh
./instigator share --network-tcp 127.0.0.1:45031 \
  --share "src=$HOME/code" --share out=/tmp/out
```

Each directory must already exist. In the guest these are `/src` and `/out`.
Renaming between exports fails with `XDEV`. Copy the file instead.

For host sockets:

- `--listen ADDR` binds an address. Repeat it to bind several.
- `--port PORT` changes the NFS/MOUNT port from 2049.
- `--portmap-port PORT` changes the portmap port from 111. 0 disables it.
  IRIX's `mount` asks portmap on UDP 111 even when given `port=`, so disabling
  it is not a substitute for a reachable portmap service for IRIX.
- `--udp=false` or `--tcp=false` disables that transport.
- `--verbose` logs every RPC call.

On the private network, `--address ADDR/PREFIX` changes the server address.
Repeat `--client MAC=IP,HOSTNAME` for more clients. The hostname is optional.
A single client may omit the IP and use `10.98.0.65`. Give an explicit IP when
you change the subnet or configure several clients.

## Permissions and limits

Each share is rooted at its host directory. Paths and symlinks cannot reach
outside it. The server hides the setuid bit and refuses hard links and
special-file creation. It implements neither NFSv4 nor network locking (NLM).
On IRIX, use the mount option `private` when locks should stay local. `nolock`
is ignored.

File handles survive server restarts. On Windows and filesystems without file
birth times, Instigator keeps a path table in the user cache directory.
`--handle-table FILE` selects another location. Keep that table if clients
will remount after a restart. Path tables detect replacements made through
NFS, but cannot detect a host process replacing a file at the same path. The
[NFS reference](nfs-shares.md#files-owners-and-handles) explains the handle
formats and limitations.
