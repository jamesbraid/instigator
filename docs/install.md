# Installing IRIX

Use the docs from the same release as your binary. For which machines and
releases have completed an install, see [what has been
tested](../README.md#what-has-been-tested).

## Before you start

You need:

- An Instigator binary for your host, or Go 1.27.1 or newer to build it.
- Your IRIX install images, including the matching boot and foundation media,
  or the published images the shipped configs point at.
- The server's IPv4 address on the install network and the client's MAC and IP
  addresses. At the PROM, `printenv eaddr` shows the client MAC.
- Console access to the SGI and a network connection to the server.

IRIX media is read from your local files or the URLs in the config. The
Instigator binary contains no IRIX images.

## Choose a config

The [`configs/`](../configs/) directory holds ready-made media layouts using
images published at <https://origami-dist.irix.fans/irix/>, the same media the
Origami emulator uses. You can use those URLs or point the configs at your
own copies of the discs.

| File | Release and media | Intended machines |
| --- | --- | --- |
| [irix-6.5.30-standard.yaml](../configs/irix-6.5.30-standard.yaml) | IRIX 6.5.30, all five disc sets | Octane, Origin |
| [irix-6.5.30-minimal.yaml](../configs/irix-6.5.30-minimal.yaml) | IRIX 6.5.30, overlays and foundations | Octane, Origin |
| [irix-6.5.22-standard.yaml](../configs/irix-6.5.22-standard.yaml) | IRIX 6.5.22, all five disc sets | Indy, Indigo2 (IP22) |
| [irix-6.2-standard.yaml](../configs/irix-6.2-standard.yaml) | IRIX 6.2, both discs | R4000 Indigo, Indy, Indigo2 |
| [irix-5.3-standard.yaml](../configs/irix-5.3-standard.yaml) | IRIX 5.3, the general disc | Indigo, Indy, Indigo2 |

The configs give you the media layout and package selections. 6.5.22 has no
minimal config because its overlay discs update products from the
Applications and Development discs.

To lay out your own media instead, start from the example in your release
archive or checkout and see the [configuration guide](configuration.md):

```sh
cp instigator.example.yaml instigator.yaml
```

The example uses documentation addresses and a placeholder Freeware URL.
Replace those values and remove sets you will not use. Keep the install sets
in the [tested order](#tested-6530-install-set-ordering).

### Use local media

Replace each `source:` with a local image or extracted-directory path. If your
local directory has the same layout as the remote collection, you can replace
the URL prefix:

```sh
sed 's#https://origami-dist.irix.fans/irix/#/srv/irix/#' instigator.yaml > local.yaml
```

Update the sources in any enabled add-on files separately. The [configuration
guide](configuration.md) covers archive subdirectories, merge collisions, and
other layouts.

### Add compilers or drivers

The IRIX 6.5 configs can install extra software after IRIX. Enable `addons:`
in your copied config:

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
each named script. Load `/irix.cmds` to install them. The built-in
`/install.cmds` does not install add-ons. IRIX 5.3 and 6.2 have no add-ons.

## Set up, check and serve

From the unpacked release archive or checkout, copy the config for your
machine. For an Octane:

```sh
cp configs/irix-6.5.30-standard.yaml instigator.yaml
cp -R configs/addons .
```

Copying `addons/` keeps the config's relative paths working if you enable
add-ons. Then edit `instigator.yaml`:

- Set `server_ip` to the server's IPv4 address on the install network.
- Set `netmask` to that network in CIDR form, such as `192.0.2.0/24`.
- Set the client's MAC and IP. At the PROM, `printenv eaddr` shows the MAC.

The supplied addresses are placeholders. Check the config before binding
network ports:

```sh
./instigator check instigator.yaml
```

For sets made entirely from local media, `check` opens the sources, checks
merge collisions and named product selections, and generates the installer
files. For a set with remote sources, it validates the layout and probes the
URLs without reading the media. To assemble remote sets too, run:

```sh
./instigator check --full instigator.yaml
```

A full check can download archives and read remote images. `serve` performs
the same assembly at startup. Neither mode binds install ports, runs the
installer, or proves the media will boot your machine.

Startup time and disk use depend on the chosen media. [Remote
sources](configuration.md#remote-sources) explains what is downloaded and
cached.

Start the server on Linux or macOS:

```sh
sudo ./instigator serve instigator.yaml
```

On Windows, run `instigator.exe serve instigator.yaml` as Administrator. For
firewall ports and hosts with several network interfaces, see [network
setup](operations.md#network-setup). Leave the server running throughout the
install.

## Machines and boot files

| Machine | PROM | Boot file | Releases | At `Inst>` |
| --- | --- | --- | --- | --- |
| Octane, Origin | 64-bit | `stand/fx.64` | 6.5.30 | `admin source <server-ip>:/irix.cmds` |
| Indy, Indigo2 (IP22) | 32-bit ARCS | `stand/fx.ARCS` | 6.5.22 | `admin source <server-ip>:/irix.cmds` |
| Indy, Indigo2 (IP22) | 32-bit ARCS | `stand/fx.ARCS` | 6.2, 5.3 | type `from`, `install default`, `go` |
| R4000 Indigo | 32-bit ARCS | `stand/fx.ARCS` | 6.2, 5.3 | type `from`, `install default`, `go` |
| R3000 Indigo | pre-ARCS | `stand/fx.IP12` | 5.3 | type `from`, `install default`, `go` |

With the example config instead of a shipped one, IRIX 6.5 loads
`/install.cmds` instead of `/irix.cmds`. The 5.3 disc carries `fx.IP12` as
well as `fx.ARCS`. The 6.5.22 Overlays 1 disc holds `miniroot/unix.IP22` and
`ide.IP22`.

## Prepare the disk

Disk partitioning depends on your target machine and is not covered here. If
the disk needs preparing, choose Enter Command Monitor at the System
Maintenance Menu and boot the `fx` partitioner. Use your client IP, the boot
file from the table, and the name of your first enabled set that is not an
add-on (`6.5.30`, `6.5.22`, `6.2` or `5.3` in the shipped configs):

```text
setenv netaddr 192.0.2.30
boot -f bootp():/6.5.30/stand/fx.64
```

After preparing the disk, quit `fx` and return to the System Maintenance
Menu. If the disk is already prepared, start at that menu.

## Start the installer

At the System Maintenance Menu, choose the software installation option. Its
wording varies by PROM, such as Install Software or Install System Software.
Then choose Remote Directory and enter the server IP and the first set's
distribution, without a trailing slash:

```text
/6.5.30/dist
```

The PROM boots the installer by fetching `dist/sa` over TFTP. With 6.5.22 on
IP22 it selects `sashARCS`, which fetches `/6.5.22/dist/miniroot/unix.IP22`.

## At Inst>

### IRIX 6.5

With a shipped config, load its script at `Inst>`:

```text
admin source 192.0.2.10:/irix.cmds
```

That script includes the config's package choices and any files listed under
`addons:`. With the example config, load the baseline `/install.cmds`
instead. It does not install add-ons.

The command file opens every enabled supplemental set and reopens the primary
release last. If `inst` opens an `Install software from` menu, enter the
number beside `done`. The command file then selects the standard product set
and starts the install. It keeps `java_dev.sw.base` deselected so the Java
runtime can be installed without that conflict.

For a selection-only check in `inst`, enter the selection commands manually
and stop before the final `admin source` handoff, which starts `go`.
`instigator check` validates the server configuration. It does not resolve
installer dependencies.

#### Named install scripts

If the configuration defines `install_scripts`, each one is served next to the
default at `/<name>.cmds`. Load a named script instead of the default to get
its extra selections. For example, a `debug` script can also install `dbx`:

```text
admin source 192.0.2.10:/debug.cmds
```

The named script opens the same sets in the same order, then applies the
standard selection plus that script's own `install`/`keep`/`remove` lines and
release stream.

#### Add-on second pass

A script with a `then:` pass installs add-on sets after the operating system.
Its first `go` installs from every enabled set that is not an add-on. When
that `go` returns to `Inst>`, the script opens the add-on distributions, runs
`keep *`, selects the configured add-on products and runs `go` again. Opening
an add-on distribution at this point does not show the `Install software from`
menu. After the second `go` returns, `quit` and answer the restart prompt as
usual. An IRIX 6.5.30 install of the PsiTech RAD4 driver this way relinked the
kernel with the driver during `quit`'s automatic reconfiguration, and the
first boot loaded it.

#### Post-install commands

A script with `post_install` commands sources them after its last `go`
returns, after the second pass when it has one. The file sets `inst`'s `shell`
preference to `/bin/sh` and runs each command with `shroot`, which runs it
inside the installed system. `shroot` fails before `go` because the target
does not exist yet. The first failing command prints `(command failed)` and
returns to `Inst>` without running the rest. When every command succeeds, the
file sets `shell` back to the default `/bin/csh` and the console shows
`instigator: post-install commands finished` before `Inst>`.

`inst` saves preferences in the installed system's `/var/inst/.swmgrrc`, so
after a failed command later `inst` sessions there keep `shell` at `/bin/sh`
until it is set back.

#### Tested 6.5.30 install-set ordering

For a `6.5.30` installation, keep the enabled sets in this order:

1. `6.5.30` overlays, with all overlay discs merged into one set.
2. `foundations`, with Foundation and NFS media merged together.
3. `development`, containing Development Foundation/Libraries.
4. `applications`.
5. `complementary`.
6. `freeware`, when enabled.

The generated command file opens the supplemental sets in that order and
reopens `6.5.30` last. That ordering comes from the successful Octane run.

Applications and Complementary Applications remain separate sets. Multiple
discs within a set are merged into one logical `/<set>/dist` tree. Disc names
are never exposed to `inst`.

### IRIX 5.3 and 6.2

Their `inst` has no `admin source`, so these configs have no `/irix.cmds`,
named scripts or add-ons, and the install records no timing. If you add
`install_scripts` to such a config, `serve` warns at startup that `inst`
cannot load them.

In the miniroot, `admin mkfs` erases the root and `/usr` filesystems for a
clean install. 6.2's `mkfs` also asks whether to make each one EFS or XFS.
6.2 asks for the machine's network address if the disk has none, and offers
an optional startup script before `Inst>`. The script checks for COFF programs
and old kernel files. At `Inst>`, type:

```text
from 192.0.2.10:/5.3/dist
install default
go
```

For 6.2, use `/6.2/dist`. Its two discs form one distribution, so one `from`
opens all of 6.2.

An Indy with an R4600 or R5000 needs the "5.3 for Indy including R5000" disc
instead of the general one. Replace the layer's source with
`https://origami-dist.irix.fans/irix/5.3/indy-r5000.iso`.

## Finish and check the installation

When the final `go` returns, check the console for errors, then enter `quit`.
Allow automatic reconfiguration to complete before restarting. A timed return
from `go` only means the command returned. Check the installed system and its
first boot to confirm success.

At the restart prompt, use its shell before rebooting if you want to inspect
the target root mounted at `/root`:

```sh
ls -l /root/unix /root/usr/stand/ide
dvhtool -v list /dev/rdsk/dks0d1vh
```

The volume header should contain `sash`, `ide`, and the machine PROM. The
installed root should contain `/unix`. The PROM normally loads `sash` from the
volume header, not from `/stand` in the root filesystem.

For common failures, see [troubleshooting](operations.md#troubleshooting).
For capture logs, install timing, and emulator disconnects, see the
[operations guide](operations.md). For script settings, see the
[configuration guide](configuration.md).
