# IRIX network install

The documented hardware install is an Octane2 running IRIX 6.5.30. It
netbooted, installed from merged media, booted from disk, and reached the
console login. Other machines and releases need their own boot files and
validation.

Use the docs from the same release as your binary. The steps below cover
serving the media and loading installer selections. Disk partitioning depends
on your target machine and is not covered here.

## Before you start

You need:

- An Instigator binary for your host, or Go 1.27.1 or newer to build it.
- Your IRIX install images, including the matching boot and foundation media.
- The server's IPv4 address on the install network and the client's MAC and IP
  addresses. At the PROM, `printenv eaddr` shows the client MAC.
- Console access to the SGI and a network connection to the server.

IRIX media is read from your local files or the URLs in the config. The
Instigator binary contains no IRIX images.

## Configure and start the server

For a ready-made media layout, start with the [shipped
configs](real-hardware.md). To configure your own media, copy the example from
your release archive or checkout:

```sh
cp instigator.example.yaml instigator.yaml
```

Edit `server_ip`, `netmask`, the client entry, and the media sources. The
example uses documentation addresses and a placeholder Freeware URL. Replace
those values and remove sets you will not use. `enabled: false` only keeps a
set out of generated installer selections. The server still opens its media.

Keep the install sets in the order [listed
below](#tested-6530-install-set-ordering). The first enabled set that is not
an add-on is the primary release. For the example, `<primary-set>` is
`6.5.30`. Replace `<client-ip>` and `<server-ip>` in the console commands with
your configured addresses.

Validate the configuration and media before binding network ports:

```sh
./instigator check instigator.yaml
```

For sets made entirely from local media, this opens the sources, checks
collisions and named product selections, and generates installer files. For a
set with remote sources, it validates the layout and probes the URLs without
reading the media. To assemble remote sets too, run:

```sh
./instigator check --full instigator.yaml
```

A full check can download archives and read remote images. Neither mode runs
an installation or proves the media will boot your machine.

Start the server on Linux or macOS:

```sh
sudo ./instigator serve instigator.yaml
```

On Windows, run `instigator.exe serve instigator.yaml` as Administrator. Allow
UDP 67/69, the configured TFTP transfer-port range, and TCP 514 through the
firewall. If your host has several network interfaces, check that BOOTP
broadcast replies leave through the install network. See [network
setup](operations.md#network-setup) for details.

Leave the server running while you install. It prints the boot path and remote
distribution directory for your configuration.

## Prepare the disk and boot the installer

If you need to prepare the disk, boot the `fx` partitioner from the Octane
PROM command monitor (for IP22, use the [32-bit boot
instructions](real-hardware.md#indy-and-indigo2-ip22-irix-6522)):

```text
setenv netaddr <client-ip>
boot -f bootp():/<primary-set>/stand/fx.64
```

After preparing the disk, quit `fx` and return to the System Maintenance
Menu. If the disk is already prepared, start at that menu. Choose the
software installation option, then Remote Directory. Enter the server IP
and the primary distribution without a trailing slash:

```text
/<primary-set>/dist
```

## Load the install selections

With the example configuration, load the baseline command file at `Inst>`:

```text
admin source <server-ip>:/install.cmds
```

The shipped configs use `/irix.cmds` instead. That script includes their
package choices and any files listed under `addons:`. The baseline
`/install.cmds` does not install those add-ons.

The command file opens every enabled supplemental set and reopens the primary
release last. If `inst` opens an `Install software from` menu, enter the
number beside `done`. The command file then selects the standard product set
and starts the install. It keeps `java_dev.sw.base` deselected so the Java
runtime can be installed without that conflict.

For a selection-only check in `inst`, enter the selection commands manually
and stop before the final `admin source` handoff, which starts `go`.
`instigator check` validates the server configuration. It does not resolve
installer dependencies.

### Named install scripts

If the configuration defines `install_scripts`, each one is served next to the
default at `/<name>.cmds`. Load a named script instead of the default to get
its extra selections. For example, a `debug` script can also install `dbx`:

```text
admin source <server-ip>:/debug.cmds
```

The default `/install.cmds` remains available, so choose a script by sourcing
its path. The named script opens the same sets in the same order, then applies
the standard selection plus that script's own `install`/`keep`/`remove` lines
and release stream.

### Add-on second pass

A script with a `then:` pass installs add-on sets after the operating system.
Its first `go` installs from every enabled set that is not an add-on. When
that `go` returns to `Inst>`, the script opens the add-on distributions, runs
`keep *`, selects the configured add-on products and runs `go` again. Opening
an add-on distribution at this point does not show the `Install software from`
menu. After the second `go` returns, `quit` and answer the restart prompt as
usual. An IRIX 6.5.30 install of the PsiTech RAD4 driver this way relinked the
kernel with the driver during `quit`'s automatic reconfiguration, and the
first boot loaded it.

### Post-install commands

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

## Tested 6.5.30 install-set ordering

For the tested `6.5.30` installation, keep the enabled sets in this order:

1. `6.5.30` overlays, with all overlay discs merged into one set.
2. `foundations`, with Foundation and NFS media merged together.
3. `development`, containing Development Foundation/Libraries.
4. `applications`.
5. `complementary`.
6. `freeware`, when enabled.

The generated command file opens the supplemental sets in that order and
reopens `6.5.30` last. That ordering is based on the successful Octane run.

Applications and Complementary Applications remain separate sets. Multiple
discs within a set are merged into one logical `/<set>/dist` tree. Disc names
are never exposed to `inst`.

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

 For capture logs, install timing, emulator disconnects, and service setup,
see the [operations guide](operations.md). For script settings, see the
[configuration guide](configuration.md).
