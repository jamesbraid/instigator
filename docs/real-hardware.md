# Installing IRIX on real hardware

`configs/` holds ready-made configurations that install from the disc images
published at <https://origami-dist.irix.fans/irix/>. Each is an ordinary
Instigator config; copy one and edit the client.

| File | Release | Machines |
| --- | --- | --- |
| `irix-6.5.30-standard.yaml` | IRIX 6.5.30, all five disc sets | Octane, Origin |
| `irix-6.5.30-minimal.yaml` | IRIX 6.5.30 overlays and foundations only | Octane, Origin |
| `irix-6.5.22-standard.yaml` | IRIX 6.5.22, all five disc sets | Indy, Indigo2 (IP22) |

6.5.22 has no minimal profile: its overlay discs update products whose
originals are on the Applications and Development discs.

## Set up the server

1. Copy the file and its `addons/` directory if you want add-ons.
2. Under `clients:` set the machine's MAC (`printenv eaddr` at the PROM) and
   the IP it should use. Set `server_ip` and `netmask` to the server's address
   on the install network.
3. Check every URL, then serve (root, for UDP 67 and 69):

   ```sh
   instigator check irix-6.5.22-standard.yaml
   sudo instigator serve irix-6.5.22-standard.yaml
   ```

   The first run downloads about 4.3 GB into the cache directory; later runs
   reuse it. `serve` prints the PROM and `Inst>` commands for the client.

To install from your own copies of the discs, point each `source:` at a local
path. If you mirror that URL directory with the same layout, only the prefix
changes:

```sh
sed 's#https://origami-dist.irix.fans/irix/#/srv/irix/#' irix-6.5.22-standard.yaml > local.yaml
```

A source can be a disc image file or an extracted directory.

## Add-ons

List add-on files under `addons:` to install extra software after IRIX, in a
second `go` of the `irix` script. An add-on file has a `name`, `layers` like
an install set's, and the `install` products to select from them.
`configs/addons/mipspro.yaml` installs the MIPSpro 7.4.4 compilers;
`configs/addons/rad4.yaml` installs the PsiTech RAD4 driver for PCI systems
and does not apply to an Indy or Indigo2.

```yaml
addons:
  - addons/mipspro.yaml
```

Paths are relative to the config file. Each add-on becomes an install set
named `addon-<name>`, and every script in `install_scripts` gains the
second pass. The built-in `install.cmds` has no add-ons; load `irix.cmds`.

## Indy and Indigo2 (IP22), IRIX 6.5.22

IP22 is untested on hardware. Its PROM runs 32-bit ARCS programs, so the
boot file is `stand/fx.ARCS`, not `fx.64`. The IP22 miniroot
(`miniroot/unix.IP22`) and `ide.IP22` are on the 6.5.22 Overlays 1 disc.

At the System Maintenance Menu choose Enter Command Monitor, then:

```text
setenv netaddr 192.0.2.30
boot -f bootp():/6.5.22/stand/fx.ARCS
```

At `Remote Directory`, enter `/6.5.22/dist`. When `Inst>` appears:

```text
admin source 192.0.2.10:/irix.cmds
```

If `inst` opens an `Install software from` menu, enter the number beside
`done`. The PROM's own System Maintenance Menu route also works: Install
Software, Remote Directory, the server's IP and `/6.5.22/dist`. The PROM
fetches `/6.5.22/dist/sa` over TFTP and picks its `sashARCS` entry; sash
then fetches `/6.5.22/dist/miniroot/unix.IP22`.

The addresses above are the placeholders from the config.

## Octane and Origin, IRIX 6.5.30

```text
setenv netaddr 192.0.2.30
boot -f bootp():/6.5.30/stand/fx.64
```

At `Remote Directory`, enter `/6.5.30/dist`, then at `Inst>`:

```text
admin source 192.0.2.10:/irix.cmds
```

See [the installation guide](install.md) for the miniroot, install timing and
first-boot checks.
