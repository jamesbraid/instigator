# instigator

<p align="center">
  <img src="assets/logo.png" alt="instigator — SGI, IRIX, and Go connecting an install" width="280">
</p>

IRIX network installs usually start with prepared media: extract the discs,
construct a network tree, arrange the boot files, and make the installer see
the right sources. That preparation is easy to get subtly wrong and makes
experimentation expensive.

`instigator` is a single binary that serves an install directly from SGI CD
images. Point it at the images, give it a small YAML file, and it provides the
BOOTP, TFTP, and rsh services needed by the SGI PROM and `inst` session. The
images remain read-only and in place.

Multiple discs can be grouped into one install set. `instigator` reads the SGI
volume header and EFS filesystem directly, then presents the selected
distribution directories as one coherent tree. The installer sees the merged
tree, not the individual discs. Duplicate files must agree, and real
collisions are named in the configuration instead of being silently guessed.

The current profile has completed on a real Octane2 with IRIX 6.5.30: the
machine netbooted, installed from merged media, booted from its disk, and
reached the IRIX console login.

## Quick start

Build the server with Go 1.27.1 or newer:

```sh
go build -o instigator ./cmd/instigator
```

`./instigator --version` prints the module version recorded by Go, including
`+dirty` for local changes. Builds without source metadata report `(devel)`.

Copy [`instigator.example.yaml`](instigator.example.yaml), then set the
server address, the client MAC/IP, and the paths to your SGI images. Keep
layers in install order. The example shows the tested 6.5.30 profile,
including merged overlays, Foundations, Development Foundation/Libraries,
Applications, Complementary Applications, and optional Freeware.

Check the configuration before starting the server. No network ports are
bound. Install sets built only from local files and directories are
assembled in full: every source is opened, merge collisions and named script
product selections are checked, and the installer files are generated. A
selected product must have a `.sw` or `.man` file in an enabled
distribution. Local archive sources may be extracted into the configured
cache.

A set with any `http://` or `https://` source is checked without reading its
media. Its layout is validated, its local sources must exist, and each URL
gets one HEAD request, or a one-byte ranged GET when the server refuses HEAD.
A URL passes on a 200 or 206 that does not report an empty file. Up to eight
probes run at once, each limited to 30 seconds, and every failure is listed
by URL. The disc contents, collisions and product selections of such a set
are checked when `serve` assembles it at start.

```sh
./instigator check instigator.yaml
```

`check --full` assembles every set as `serve` does, downloading remote
archives into the cache and reading every remote disc image.

Start the server on the install network:

```sh
sudo ./instigator serve instigator.yaml
```

At the Octane PROM command monitor, boot the configured set:

```text
setenv netaddr <octane-ip>
boot -f bootp():/<primary-set>/stand/fx.64
```

At `Remote Directory`, enter `/<primary-set>/dist`. At `Inst>`, load the
generated selections and start the install:

```text
admin source <server-ip>:/install.cmds
```

If `inst` opens an `Install software from` menu, enter the number beside
`done`. The command file resumes its package selections and starts the install.

The [installation guide](docs/install.md) has the full command sequence,
profile ordering, first-boot checks, and captured install notes.

## Host platforms

The server is pure Go and runs on Linux, macOS, and Windows. Real installs have
been proven on Linux (the Octane2 run above). macOS and Windows are supported
build and test targets.

Binding UDP 67 (BOOTP) and 69 (TFTP) needs privilege. Use `sudo` on Linux and
macOS. On Windows, run as Administrator and
allow UDP 67/69 and TCP 514 through the firewall. The installation guide covers
the multi-homed-host trap that bites laptops.

## Configuration

`install_sets` are the logical trees exposed to `inst`. Each set contains
ordered `layers`, and each layer names one `source`: a local path or an
`http(s)://` URL.

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
archive. Local archives are extracted into `cache_dir` for the serve run and
removed when the source closes. `base:` names the subtree holding the
install files. For an archive containing `disc1/dist/`, use `base: disc1`.
`dist:` and `stand:` resolve under `base` and default to `dist` and
`stand`. Set `dist:` for a media directory such as `dist6.5`. `boot: true` marks the one layer
whose `stand/` files are served to the PROM. `collisions` records an
explicit winner when two layers contain different bytes at the same logical
path. Identical duplicates are accepted.

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

### Remote sources

`source:` also accepts an `http(s)://` URL. A `.tar.gz`/`.tgz`/`.tar`/`.gz`
archive is downloaded and unpacked. A raw `.image` on a range-capable server
is read lazily, pulling only the bytes an install touches. `credentials:`
supply host-matched HTTP Basic auth over HTTPS, with a `${VAR}` password read
from the environment. Downloads are cached under `cache_dir:` (default: the
user cache dir) and reused across runs. Each run re-extracts an archive fresh.

The complete example also shows client filtering, service toggles, and the
low TFTP transfer-port range required by SGI PROMs.

### Install scripts

By default `instigator` serves one command file, `/install.cmds`, that selects
the standard product set. `install_scripts` add named variants that select more
on top of that baseline — for example a `debug` script that also installs the
`dbx` debugger:

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
resolves them against the open media — so a name must exist on some enabled set,
and you can confirm its exact spelling with `inst`'s `list`. `stream` picks the
IRIX release stream: the default feature stream, or `maintenance` for the
fixes-only maintenance overlays. `install` is reserved for the baseline, so no
script may take that name.

### Add-on sets

Third-party products that install into paths the operating system makes
symbolic links, such as a graphics driver writing below `/var/sysgen/boot`,
must be installed after the operating system. Installed in the same `go`,
`inst` can create a real directory first, and the link the operating system's
own product makes later is never created. Mark such a set `addon: true` and
install from it in a script's `then:` pass:

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
`then.install` name to have a `.sw` or `.man` file in them. For an add-on
set with remote sources, only `instigator check --full` reads the media to
confirm this.

### Post-install commands

`post_install` runs shell commands in the installed system after the script's
last `go`, so one-line settings need no console session:

```yaml
install_scripts:
  - name: dhcp
    post_install:
      - chkconfig -f autoconfig_ipaddress on
```

Each command is one line, run by `/bin/sh` through `inst`'s `shroot`, which
runs it chrooted into the new system. A failing command prints
`(command failed)` and `inst` skips the remaining commands. After the last one
succeeds, `inst`'s `shell` preference goes back to `/bin/csh` and the console
shows:

```text
instigator: post-install commands finished
```

## Extracting installed files

`instigator extract` writes the files inst products would install, and a
manifest of every entry, to a host directory:

```sh
instigator extract [--path PATH]... [--subsystem GLOB]... [--unpack | --manifest-only] [--dist DIR] [--name NAME] <image|dir>... <outdir>
instigator extract [--set NAME]... [--path PATH]... [--subsystem GLOB]... [--unpack | --manifest-only] <config.yaml> <outdir>
```

With images or directories, each is a layer of one install set, in the order
given, merged with the default strict rules; layers that disagree need a
config file. `--dist` names the distribution directory in every source
(`dist` by default, `.` for the source root, as in an unpacked tardist), and
`--name` the output, by default the first source's name without its
extension. A single `.yaml` or `.yml` source is a config file, a full serve
configuration or one holding only `install_sets:`; `--set` picks sets by name,
and by default all are extracted. A set is built as when serving, and may
name the sets it is installed after, which `serve` ignores:

```yaml
install_sets:
  - name: base
    layers: [...]            # the base release's discs
  - name: overlay
    after: [base]            # transitive; unknown names and cycles are refused
    layers: [...]            # the overlay release's discs
```

Extracting `overlay` then writes one installed view: the sets it is
installed after, in order, then the set itself. A product a later set ships
again replaces the earlier product whole, and a later set wins any other
install path it installs; the entries it replaces are `superseded` and never
read. Where one set installs a directory and a later one a file or link, the
later entry is an `error`. This is not a full `inst` run: files an overlay
retires without reinstalling stay.

```sh
instigator extract --path usr/share/catman disc.image out/
```

writes `out/disc/` and `out/disc.jsonl`.

- `--path PATH` keeps only entries at or under the install path `PATH`,
  matched by whole components: `usr/include` keeps `usr/include/stdio.h` but
  not `usr/include2`. Repeat it for several.
- `--subsystem GLOB` keeps only entries whose subsystem matches the shell
  pattern `GLOB` (Go's `path.Match`), such as `*.man.*` for every manual
  page subsystem. Repeat it for several. With `--path` too, an entry must
  pass both. Entries left out are neither listed nor read.
- `--unpack` also removes compression on the installed file itself: a `.z`
  name holding pack data or a `.Z` name holding compress data loses its
  suffix, and links to it follow (`man/b.z -> a.z` becomes `man/b -> a`). The
  archive's own compression is always removed.
- `--manifest-only` writes only `<outdir>/<set>.jsonl`, planned from the
  `.idb` files without opening any archive. Entries that would be extracted
  have status `planned` and no `path`, `sha256` or `target`; whether one
  would be written, a duplicate, a variant or an error needs its data.
  Filters and `after` sets apply as usual. It cannot be combined with
  `--unpack`.

Files keep their `.idb` permission bits plus owner access; nothing is
chowned. Device, socket and type `X` entries, and `delhist` entries (paths an
older release installed), are recorded but not created. The manifest has one
JSON object per entry, in product then `.idb` order, with empty fields
omitted:

| Field | Meaning |
| --- | --- |
| `path` | where the entry was written, relative to `<set>/` |
| `installPath` | the path as written in the `.idb` |
| `type`, `mode`, `owner`, `group` | from the `.idb` |
| `product`, `subsystem` | owning product and subsystem |
| `layer` | source layer holding a file's archive, or else the product's `.idb` |
| `set` | the install set that supplied the entry |
| `sha256` | digest of the bytes written |
| `symlink`, `target` | link target as in the `.idb`, and as created |
| `mach` | the `mach()` attribute |
| `status` | `written`, `duplicate`, `variant`, `planned`, `superseded`, `skipped`, `error` or `unparsed` |
| `error` | the reason, with the line number for `unparsed` |

A path installed again with the same content is a `duplicate`. Anything else,
such as a per-machine copy, is a `variant` written beside it as
`<path>.<mach>`, or `<path>.<first 8 hex digits of its sha256>` when it has
no `mach` or that name would exceed 255 bytes. An entry that cannot be
extracted, for example because its archive's disc is not in the set, is an
`error`; the run carries on and exits non-zero. Existing output is never
overwritten.

## Sharing a host directory

`instigator share` exports a host directory to IRIX, read-write, over NFSv2
and NFSv3 on UDP and TCP, with MOUNT and portmap beside them. It runs until
interrupted and is separate from `serve`: nothing on the install path uses
it. To give a machine on a private network shares, see "On a private
network" below.

```sh
instigator share --listen 192.168.0.1 src ~/code
```

exports `~/code` as `/src`. The guest sees every file owned by root;
changes land owned by the user running `instigator`, and a chown changes
nothing. The setuid bit is never shown or set, and a write clears it. The
server never reaches outside the directory, through `..` or a symlink.
LINK and MKNOD are refused.

- `--listen ADDR` listens on `ADDR` (default `127.0.0.1`). Repeat it for
  several.
- `--port PORT` serves NFS and MOUNT on one port (default 2049). Every port
  answers all three programs, so a client told the port needs no portmap.
- `--portmap-port PORT` serves portmap (default 111, which needs privilege
  on most hosts); 0 serves none.
- `--udp=false` or `--tcp=false` drops a transport.
- `--handle-table FILE` keeps filehandles in a table of paths instead of
  deriving them from inodes. Windows has no usable inode numbers, so there
  it defaults to a file in the user cache directory. So does a share on a
  filesystem that keeps no file birth times, an NFS mount for one: without a
  birth time a removed name's inode comes back for the next file, and an
  inode handle could not tell them apart. Either way a handle survives a
  restart.
- `--share NAME=DIR` adds another export (repeatable), beside or instead of
  the positional `<name> <dir>`. One server answers for all of them, and a
  rename between two refuses with `XDEV`.

### On a private network

With `--network-socket PATH` or `--network-tcp IP:PORT` the shares are
served on the private Ethernet segment a QEMU machine connects to, the one
`serve` uses for installs, and nothing is bound on the host. The machine
reaches them at the server's address on ports 111, 635 and 2049, over UDP
and TCP, so IRIX's `mount` finds them through portmap:

```sh
instigator share --network-tcp 127.0.0.1:45031 --share src=~/code --share out=/tmp/out
# on the guest, with ef0 up and an address on 10.98.0.0/24
mount 10.98.0.2:/src /mnt/src
```

`--address ADDR/PREFIX` sets the server's address (default `10.98.0.2/24`).
`--client MAC=IP[,HOSTNAME]` (repeatable) also gives that machine its address
by BOOTP and DHCP, so an installed IRIX that asks for one gets it with no
`ifconfig`; the IP defaults to 10.98.0.65 for a single client.
`--listen`, `--port`, `--portmap-port`, `--udp` and `--tcp` do not apply.
The command exits when the machine disconnects. `serve` takes the same
`--share NAME=DIR` beside its install services, off unless given; it needs
`--network-socket` or `--network-tcp`. See the "Private network" section of
[docs/nfs-shares.md](docs/nfs-shares.md) for the segment.

## Development

Run the test suite with:

```sh
go test ./...
```

The tests cover the EFS reader, merged virtual trees, collision handling,
generated `inst` commands, BOOTP, TFTP, rsh, and capture behavior. Tests that
need local IRIX media skip when that media is unavailable:

```sh
go test -run RealMedia -v ./internal/vfs ./internal/instcmd ./cmd/instigator
```

Captures retain request, transfer, and timing data from real runs, so a later
install can be compared against a known-good one.

## License

MIT
