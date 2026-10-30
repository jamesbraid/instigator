# Configuration

For ready-made remote media layouts, use the [shipped
configs](real-hardware.md). For your own layout, start with
[instigator.example.yaml](../instigator.example.yaml). Copy it to
`instigator.yaml` and edit the addresses and media paths before running it.
The example contains placeholder addresses and a placeholder Freeware URL.

For your first install, follow the [installation guide](install.md). This page
explains how to change the media layout and generated installer scripts.

## Client and network addresses

`server_ip` is the server address the SGI should contact. `netmask` is a CIDR
network such as `192.0.2.0/24`. Each `clients` entry names a MAC and the IPv4
address to offer it. The optional `hostname` is sent as BOOTP/DHCP option 12.
It is separate from `name`, which labels the client in logs.

On a host interface, Instigator answers BOOTP for configured clients and
ignores DHCP. On the private emulator network it answers both. See the
[operations guide](operations.md) for network setup.

`dhcp_server_id` sets the DHCP server identifier, which defaults to
`server_ip`. If an installed IRIX guest moves between private and QEMU user
networks, give both the same identifier so `proclaim` accepts offers after
the move. With Origami's default user network, use:

```yaml
dhcp_server_id: 192.0.2.2
```

This changes DHCP option 54, not the server's address or BOOTP replies.
See [moving between networks](sharing.md#moving-between-networks) for the
lease behavior and limits.

## Sets and layers

`install_sets` are the distribution trees exposed to `inst`, the IRIX
installer. Each set contains ordered `layers`, and each layer names one
`source`: a local path or an `http(s)://` URL.

```yaml
install_sets:
  - name: "6.5.30"
    layers:
      - name: overlays1
        source: /media/irix/overlays1.image
        boot: true
      - name: overlays2
        source: /media/irix/overlays2.image
```

`instigator` auto-detects whether a source is an SGI image, an extracted
directory, or a local `.tar`, `.tar.gz`, `.tgz`, `.tardist`, or `.tardist.gz`
archive. Local archives are extracted under `cache_dir` and the extracted
files are removed when the source closes. `base:` names the subtree holding
the install files. For an archive containing `disc1/dist/`, use `base: disc1`.
`dist:` and `stand:` resolve under `base` and default to `dist` and `stand`.
Set `dist:` for a media directory such as `dist6.5`.

`boot: true` marks the layer whose `stand/` files are served to the PROM. Each
set can have at most one boot layer. The first enabled set that is not an
add-on is the primary set. Put the release you want to boot first.

`collisions` names the winning layer when two layers contain different bytes
at the same logical path. Identical duplicates are accepted. A collision key
is the full served path without a leading slash, such as
`foundations/dist/inst.README` in the example.

Every configured set is built and served. `enabled: false` leaves a set
browsable but excludes it from the generated installer scripts. Remove a set
from the configuration if you do not have its media. Disabling it does not
skip opening its sources.

When a later layer updates files from one earlier layer, list both in that
order and name the pair under `replacements`:

```yaml
install_sets:
  - name: development
    layers:
      - name: foundation
        source: /media/foundation.image
      - name: update
        source: /media/update.tar.gz
    replacements:
      foundation: update
```

The later layer wins differing files shared by this pair. An exact
`collisions` winner takes priority. Differing files from other layer pairs
still stop the build.

## Remote sources

`source:` also accepts an `http(s)://` URL. Tar archives are downloaded and
unpacked into a directory. A bare `.gz` source is decompressed into an image.
A raw `.image` on a range-capable server is read lazily, pulling only the
bytes an install touches. `credentials:` supply HTTP Basic auth over HTTPS for
the matching host. A `${VAR}` password is read from the environment. Downloads
are cached under `cache_dir:` (default: the user cache dir) and reused across
runs. Each run extracts the archive into a fresh temporary directory.

The example also shows client filtering, service toggles, and the low TFTP
transfer-port range required by SGI PROMs. Keep that range when editing it.

## Install scripts

By default `instigator` serves `/install.cmds`, which selects the standard
product set and starts installation. `install_scripts` add named variants that
select more on top of that baseline. For example, a `debug` script can also
install the `dbx` debugger:

```yaml
install_scripts:
  - name: debug
    install: [dbx.sw.dbx]
  - name: maintenance
    stream: maintenance
```

Each entry is served next to the default at `/<name>.cmds`, so the one above
gives `/debug.cmds` and `/maintenance.cmds`. `/install.cmds` is always
generated and unchanged. At the `Inst>` prompt, load the one you want instead
of the default:

```text
admin source <server-ip>:/debug.cmds
```

`install`, `keep`, and `remove` add those `inst` selection lines after the
standard set. `instigator` passes the names straight through to `inst`, which
resolves them against the open media. A name must exist on an enabled set, and
you can confirm its exact spelling with `inst`'s `list`. `stream` picks the
IRIX release stream: the default feature stream, or `maintenance` for the
fixes-only maintenance overlays. `install` is reserved for the baseline, so no
script may take that name.

## Add-on files

The shipped configs use `addons:` to install extra software after IRIX:

```yaml
addons:
  - addons/mipspro.yaml
```

Paths are relative to the configuration file. Copy the `addons/` directory
with it, or change the paths. The supplied files install MIPSpro 7.4.4 or the
PsiTech RAD4 driver. RAD4 applies only to PCI machines with that board.

Each add-on file contains `name`, `layers`, and `install`. Its layers follow
the same rules as an install set. Instigator creates an `addon-<name>` set and
adds its products to the second pass of every named install script. The
configuration must define at least one named script. The built-in
`/install.cmds` stays unchanged, so use `/irix.cmds` with the shipped configs.

## Add-on sets

You can also define the second pass directly instead of using add-on files.

Some add-ons write through symbolic links created by the operating system. A
graphics driver writing below `/var/sysgen/boot` is one example. Install these
products after the operating system: in a single pass, `inst` can create a
directory where the operating system needs to create a link. Mark such a set
`addon: true` and install from it in a script's `then:` pass:

```yaml
install_sets:
  - name: addon-rad4
    addon: true
    layers:
      - {name: rad4, source: https://media.example.com/rad4x_65_13k.tar.gz, dist: dist_6.5}

install_scripts:
  - name: addon-rad4
    then:
      sets: [addon-rad4]
      install: [rad4x.sw.driver, rad4x.sw.xserver, rad4x.man]
```

No first pass opens an add-on set, including `/install.cmds`. After the first
`go` returns to `Inst>`, the script opens the `then` sets, keeps everything
already installed, selects the named products and runs `go` again. `then.sets`
must name enabled add-on sets, and `instigator check` requires each
`then.install` name to have a `.sw` or `.man` file in them. For remote sets,
use `check --full` to check their contents. Ordinary `check` only probes URLs.

## Post-install commands

`post_install` runs shell commands in the installed system after the script's
last `go`, so one-line settings need no console session:

```yaml
install_scripts:
  - name: dhcp
    post_install:
      - chkconfig -f autoconfig_ipaddress on
```

Each command is one line, run by `/bin/sh` through `inst`'s `shroot`, which
runs it chrooted into the new system. A failing command prints `(command
failed)` and `inst` skips the remaining commands. After the last one succeeds,
`inst`'s `shell` preference goes back to `/bin/csh` and the console shows:

```text
instigator: post-install commands finished
```

 For console prompts, post-install failures, and service setup, see the
[installation guide](install.md) and [operations guide](operations.md).
