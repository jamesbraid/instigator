# instigator

Network install server for SGI IRIX machines. Point it at a directory of
IRIX CD images and netboot an Indy, Indigo2, O2, Octane, or Origin:
BOOTP and TFTP get the PROM into the miniroot, then rsh or NFS feeds the
distribution to `inst`.

The images are read in place. instigator parses the SGI volume header and
the EFS filesystem itself, so you never loop-mount, extract, or copy a CD
into a staging tree — the disc images are the serving tree.

**Status: the protocol stack is built and tested. No real machine has
installed from it yet.** See [Verification](#verification).

## Why

Every existing recipe (booterizer, DINA, the docker-irix images) wires
together isc-dhcp, tftpd-hpa, and an rsh daemon, then bolts on host
sysctls and a media-extraction step. instigator is one static Go binary
with no host tunables and no unpacking. It also ships a read-only
NFSv2-over-UDP server, which did not otherwise exist in a memory-safe
language — go-nfs and the Rust nfsserve crates are NFSv3/TCP, and the
only UDP option was unfs3 in C.

## Install

```
go install github.com/jamesbraid/instigator/cmd/instigator@latest
```

Or build the container (`Containerfile`), which needs host or macvlan
networking to see the client's broadcast BOOTP.

## Configure

Copy `instigator.example.yaml` and set the client MAC (from the PROM's
`printenv eaddr`), the addresses, and the media directory:

```yaml
server_ip: 192.0.2.10
netmask: 192.0.2.0/24
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 192.0.2.30}
media:
  - {name: "6.5.30", discs: /media/6.5.30}
services:
  bootp: true
  tftp: {port_range: [2048, 32767]}
  rsh: true
```

Inspect a disc image without serving:

```
instigator ls "/media/6.5.30/IRIX 6.5.30 Installation Tools.iso" /stand
```

## Run

```
sudo instigator serve instigator.yaml
```

It binds BOOTP (67/udp), TFTP (69/udp), and rsh (514/tcp) — privileged
ports, hence `sudo` or `CAP_NET_BIND_SERVICE`. At startup it prints the
disc map and the exact PROM commands to type. rsh answers only from
reserved source ports and every service answers only the configured
client IPs.

## At the machine

Full IRIX 6.5.30 is 11 discs: three Overlays, six base, two companion.
From the PROM command monitor:

```
setenv netaddr 192.0.2.30                                    # this client's ip
boot -f bootp():/6.5.30/installation-tools/stand/fx.64       # partition the disk
```

Partition with `fx`, then start the install. `inst` takes the
distribution from the server. Give it the per-disc `dist` path
(`192.0.2.10:/6.5.30/installation-tools/dist`), open each disc in turn,
resolve conflicts, and `go`. The exact disc slugs are whatever instigator
prints at startup, derived from your image filenames.

## Verification

Run the tests:

```
go test -race ./...
```

They drive every protocol over real sockets against synthetic EFS images
built in memory — a BOOTP exchange, a TFTP fetch, a `dd` over rsh, and a
portmap→mount→NFS read. No SGI-derived bytes are in this repository.

What is **not** yet verified: a real SGI PROM completing an install. The
tftp low-source-port requirement, the exact rsh command vocabulary
`inst` issues, and NFSv2 behavior against the miniroot are all validated
against the protocol specs and community reports, not yet against
hardware.

## License

MIT
