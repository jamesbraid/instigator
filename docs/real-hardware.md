# Installing IRIX on real hardware

The [`configs/`](../configs/) directory contains ready-made media layouts
using images published at <https://origami-dist.irix.fans/irix/>, the media
used by sgi-emulator / Origami. You can use those URLs or point the configs at
your own copies of the discs.

Choose a config for your machine:

| File | Release and media | Intended machines |
| --- | --- | --- |
| [irix-6.5.30-standard.yaml](../configs/irix-6.5.30-standard.yaml) | IRIX 6.5.30, all five disc sets | Octane, Origin |
| [irix-6.5.30-minimal.yaml](../configs/irix-6.5.30-minimal.yaml) | IRIX 6.5.30, overlays and foundations | Octane, Origin |
| [irix-6.5.22-standard.yaml](../configs/irix-6.5.22-standard.yaml) | IRIX 6.5.22, all five disc sets | Indy, Indigo2 (IP22) |

The completed hardware install recorded here is an Octane2 with IRIX 6.5.30.
IP22 remains untested on hardware. These configs give you the media layout and
package selections. They do not establish hardware acceptance for every
machine in the table.

6.5.22 has no minimal config because its overlay discs update products from
the Applications and Development discs.

## Set up the server

From the unpacked release archive or checkout:

```sh
cp configs/irix-6.5.30-standard.yaml instigator.yaml
cp -R configs/addons .
```

Choose the 6.5.22 config instead for an Indy or Indigo2. Copying `addons/`
preserves the relative paths if you later enable add-ons.

Edit `instigator.yaml`:

- Set `server_ip` to the server's IPv4 address on the install network.
- Set `netmask` to that network in CIDR form, such as `192.0.2.0/24`.
- Set the client's MAC and IP. At the PROM, `printenv eaddr` shows the MAC.

The supplied addresses are placeholders. Check the config, then serve it:

```sh
./instigator check instigator.yaml
sudo ./instigator serve instigator.yaml
```

On Windows, run `instigator.exe` as Administrator instead of using `sudo`. The
default check probes remote URLs without downloading the media. Use `check
--full` to read it and check merge collisions and product selections before
starting the server. `serve` performs that assembly at startup too. Neither
check binds install ports or runs the installer.

Raw images on a range-capable server are read as needed. Archives are fetched
whole and cached. Later runs reuse the downloads but extract them into fresh
temporary directories. Startup time and disk use depend on the chosen media.
Leave the server running throughout the install.

For firewall ports and hosts with several network interfaces, see [network
setup](operations.md#network-setup).

## Use local media

Replace each `source:` with a local image or extracted-directory path. If your
local directory has the same layout as the remote collection, you can replace
the URL prefix:

```sh
sed 's#https://origami-dist.irix.fans/irix/#/srv/irix/#' instigator.yaml > local.yaml
```

Update the sources in any enabled add-on files separately. The [configuration
guide](configuration.md) covers archive subdirectories, merge collisions, and
other layouts.

## Add compilers or drivers

To install extra software after IRIX, enable `addons:` in your copied config:

```yaml
addons:
  - addons/mipspro.yaml
```

The supplied [MIPSpro config](../configs/addons/mipspro.yaml) selects the
7.4.4 compilers. The [RAD4 config](../configs/addons/rad4.yaml) selects the
PsiTech driver and X server for PCI machines with a RAD4 board. RAD4 does not
apply to an Indy or Indigo2.

Add-on paths are relative to the main config. Each file becomes an install set
named `addon-<name>`, and its products are added to a second install pass in
each named script. With these configs, load `/irix.cmds` at `Inst>`. The
built-in `/install.cmds` does not install add-ons.

## Indy and Indigo2 (IP22), IRIX 6.5.22

IP22 uses 32-bit ARCS boot programs. The boot file is `stand/fx.ARCS`. The
6.5.22 Overlays 1 disc also holds `miniroot/unix.IP22` and `ide.IP22`.

If the disk needs preparation, choose Enter Command Monitor at the System
Maintenance Menu and boot the `fx` partitioner:

```text
setenv netaddr 192.0.2.30
boot -f bootp():/6.5.22/stand/fx.ARCS
```

After preparing the disk, quit `fx` and return to the maintenance menu. For
a prepared disk, start at that menu. Choose Install Software, then Remote
Directory, and enter the server IP and `/6.5.22/dist`. At `Inst>`:

```text
admin source 192.0.2.10:/irix.cmds
```

If `inst` opens an `Install software from` menu, enter the number beside
`done` to continue. The PROM boots the installer by fetching
`/6.5.22/dist/sa` over TFTP and selecting
`sashARCS`, which fetches `/6.5.22/dist/miniroot/unix.IP22`.

Replace the example addresses with your configured client and server IPs.

## Octane and Origin, IRIX 6.5.30

If the disk needs preparation, boot the `fx` partitioner at the PROM
command monitor:

```text
setenv netaddr 192.0.2.30
boot -f bootp():/6.5.30/stand/fx.64
```

After preparing the disk, quit `fx` and return to the maintenance menu. For
a prepared disk, start at that menu. Choose the software installation
option, then Remote Directory, and enter the server IP and `/6.5.30/dist`.
At `Inst>`:

```text
admin source 192.0.2.10:/irix.cmds
```

Replace the example addresses with your configured IPs. Enter the number
beside `done` if the source menu appears. Continue with the [installation
walkthrough](install.md#finish-and-check-the-installation) for reconfiguration
and first-boot checks. Disk partitioning depends on the target machine and is
outside this guide.
