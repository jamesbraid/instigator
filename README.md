# instigator

Network install server for SGI IRIX systems. Point it at a directory of IRIX CD
images and netboot your machine: BOOTP and TFTP for the PROM, rsh for `inst`.
The images are read in place — the SGI volume header and EFS filesystem are
parsed at serve time, so nothing is ever unpacked.

**Status: early development. Nothing installs a machine yet.**

## Planned shape

One static binary, one YAML config:

- `bootp` — answers only the client MACs you configure, nobody else's DHCP is
  disturbed
- `tftp` — serves boot artifacts (`fx`, `sash`, the miniroot) straight from the
  CD images, from low source ports so picky PROMs accept the transfer — no
  host sysctls
- `rsh` — the constrained command set `inst` actually uses, no general shell
- `nfs` — read-only NFSv2/v3 over UDP for `inst`'s NFS path (planned; no
  userspace NFS-over-UDP server exists in a memory-safe language today)

```yaml
interface: eth0
server_ip: 192.0.2.10
clients:
  - { name: octane, mac: "08:00:69:0e:af:12", ip: 192.0.2.30 }
media:
  - { name: "6.5.30", discs: /media/6.5.30 }
services:
  bootp: true
  tftp:  { port_range: [2048, 32767] }
  rsh:   true
```

## Prior art

- [booterizer](https://github.com/unxmaal/booterizer) — Vagrant/RPi appliance
  (isc-dhcp + tftpd-hpa + rsh-server)
- love (truhobbyist, contrib.irixnet.org) — single-binary C++ bootp/tftp/rsh
  netboot server
- [docker-irixinstall](https://github.com/frankeverdij/docker-irixinstall) —
  the classic daemons containerized

## License

MIT
