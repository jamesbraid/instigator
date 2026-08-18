# instigator

Network install server for SGI IRIX machines. Point it at a set of IRIX CD
images and netboot a 64-bit MIPS machine: BOOTP and TFTP get the PROM into
the miniroot, then rsh feeds `inst` its distribution.

The images are read in place. instigator parses the SGI volume header and
the EFS filesystem itself, so you never loop-mount, extract, or copy a CD
into a staging tree. Layers from one or more images (or a pre-extracted
directory, for the rare distribution that only exists as a tarball) are
merged into one served install-set tree per configured set — see
[Configure](#configure).

**Status: BOOTP, TFTP, and an rsh `inst` session have been proven end to
end on a real Octane2.** The four-logical-install-set layout this README
describes is still hardware-gated: it hasn't yet been the profile of a
complete captured install (all four sets loaded, conflicts resolved,
install run, machine booted). See [Verification](#verification).

## Why

Every existing recipe (booterizer, DINA, the docker-irix images) wires
together isc-dhcp, tftpd-hpa, and an rsh daemon, then bolts on host
sysctls and a media-extraction step. instigator is one static Go binary
with no host tunables and no unpacking: BOOTP, TFTP, and rsh are its own
implementations, and the served tree is assembled from untouched disc
images at startup rather than copied onto disk first.

An experimental read-only NFSv2/UDP server also lives in this repo
(`nfs/`, with `internal/nfsexport/` adapting it to the served tree for
tests). It has its own direct test coverage but never links into the
`instigator` binary and isn't part of the supported install path — rsh
is. It's kept around for possible future diskless-workstation work, not
as an alternative way to serve a distribution.

## Install

```
go install github.com/jamesbraid/instigator/cmd/instigator@latest
```

Or build the container (`Containerfile`), which needs host or macvlan
networking to see the client's broadcast BOOTP.

## Configure

Copy `instigator.example.yaml` and fill in the client MAC (from the
PROM's `printenv eaddr`), your addresses, and each install set's layers:

```yaml
server_ip: 192.0.2.10
netmask: 192.0.2.0/24
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 192.0.2.30}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: overlays1, image: /media/6.5.30/overlays1.image}
      - {name: overlays2, image: /media/6.5.30/overlays2.image, source_dir: dist, target_dir: dist}
services:
  bootp: true
  tftp: {port_range: [2048, 32767]}
  rsh: true
```

An install set is an ordered list of layers. The first layer's whole
root usually becomes the set's root (`stand`, `installtools`, and its own
`dist`); a later layer normally contributes only its own `dist`,
merged into the set's. No disc name or slug ever appears in a served
path — `inst` only ever sees `/<set-name>/dist`.

Every configured set is built and served, whether or not it's enabled;
`enabled: false` just leaves a set out of the generated commands and the
printed `Inst>` instructions, for a set you want browsable but not part
of the default install. A regular file that two layers both place at the
same path must be byte-identical, or the exact winning layer must be
named in that set's `collisions` — otherwise the server refuses to start
rather than guess. See `instigator.example.yaml` for a second set that
rebases a version-stub disc's real `dist6.5` directory onto its logical
`dist`, with a `collisions` entry resolving one such overlap.

Inspect a disc image directly, without serving it:

```
instigator ls /media/6.5.30/overlays1.image /stand
```

## Run

```
sudo instigator serve instigator.yaml
```

It binds BOOTP (67/udp), TFTP (69/udp), and rsh (514/tcp) — privileged
ports, hence `sudo` or `CAP_NET_BIND_SERVICE`. At startup it logs the
install-set inventory (every set, its layers in order, and any
configured collision winners) and prints the exact PROM and `Inst>`
commands to type. rsh answers only from reserved source ports, and every
service answers only the configured client IPs.

## At the machine

instigator generates the boot line, the `Remote Directory` value, and the
`from`/`open` sequence from your actual configuration and prints them at
startup — the commands below are what they look like for the roadmap's
full four-set profile (`6.5.30`, `foundations`, `applications`,
`development` — see [Configure](#configure)), with a client at
`192.0.2.30` and the server at `192.0.2.10`:

At the PROM command monitor:

```
setenv netaddr 192.0.2.30
boot -f bootp():/6.5.30/stand/fx.64
```

instigator only ever names a boot artifact that the primary set's first
layer actually serves — it never synthesizes one for a disc or machine
that doesn't provide it, and this line stays out of the printed
instructions entirely when no such file exists. Today that means exactly
one thing: the 64-bit `fx` (`fx.64`). There's no 32-bit `fx` path yet, so
this boots a 64-bit-capable machine; the only one exercised so far is an
Octane2. A 32-bit-only IRIX box needs its own boot artifact wired into a
future profile before this server can partition it.

At the PROM's `Remote Directory` prompt that follows, enter the primary
set's `dist` path **with the trailing slash** — the PROM's tftp request
fails before the server ever sees it if the slash is missing:

```
/6.5.30/dist/
```

`fx` partitions the disk and drops into the miniroot's `Inst>` prompt.
Open every enabled set there, in order — `from` on the first, `open` on
each one after it:

```
Inst> from 192.0.2.10:/6.5.30/dist
Inst> open 192.0.2.10:/foundations/dist
Inst> open 192.0.2.10:/applications/dist
Inst> open 192.0.2.10:/development/dist
```

Whatever discs or directories you layered into each set, `inst` only
ever sees these four logical paths — no per-disc slug. Once every set is
open, the same command sequence is available two ways: `inst.init` runs
it automatically if the miniroot picks it up on its own, and
`admin source 192.0.2.10:/install.cmds` replays the identical bytes by
hand as a fallback. Either way it clears any stale prior selection,
keeps everything, selects the standard product set, keeps incomplete
overlays, drops the Java dev kit and plugin overlays, and lists
conflicts — then stops. Review what `conflicts` reports yourself and
type `go`: nothing here resolves a conflict or starts the install for
you.

## Verification

Run the tests:

```
go test -race ./...
```

They drive BOOTP, TFTP, and rsh over real sockets — a BOOTP exchange, a
TFTP fetch, and an `inst`-style `dd` over rsh — against synthetic EFS
images built entirely in memory, plus the install-set merge and
collision logic (layer ordering, byte-identical dedup, the fail-closed
collision check, the `dist6.5` rebase) that replaces raw disc paths with
logical sets. No SGI-derived bytes are in this repository.

Two more checks are gated behind local real IRIX media and skip
themselves everywhere else:

```
go test -run RealMedia -v ./internal/vfs ./internal/instcmd
```

They check the same collision logic and the rsh `dd` path against actual
6.5.30 CD images, including the reviewed collision report for an
`applications` set built from real Applications and Complementary
Applications images.

NFS is retained as experimental source with its own direct tests, but it
never links into the binary:

```
go list -deps ./cmd/instigator | grep -E 'instigator/(nfs|internal/nfsexport)$'
```

prints nothing.

**What's hardware-verified and what isn't:** a real Octane2 has driven
BOOTP, TFTP into the miniroot, and a real `inst` session over rsh, end to
end. That proves the transport and the rsh command vocabulary `inst`
drives. It does **not** yet prove the specific four-logical-install-set
layout described in this README: that layout remains gated until a run
loads all four sets, resolves their conflicts, installs, and boots the
result. Don't read the Octane2 result as validating a layout it never
actually opened.

## License

MIT
