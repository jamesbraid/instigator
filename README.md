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

The result is a repeatable path from local image files to a real IRIX install:
one server process, one configuration, and no staging tree to prepare first.

The current profile has completed on a real Octane2 with IRIX 6.5.30: the
machine netbooted, installed from merged media, booted from its disk, and
reached the IRIX console login.

## Quick start

Build the server:

```sh
go build -o instigator ./cmd/instigator
```

Copy [`instigator.example.yaml`](instigator.example.yaml), then set the
server address, the client MAC/IP, and the paths to your SGI images. Keep
layers in install order. The example shows the tested 6.5.30 profile,
including merged overlays, Foundations, Development Foundation/Libraries,
Applications, Complementary Applications, and optional Freeware.

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

That is the short version. The [installation guide](docs/install.md) has the
full command sequence, profile ordering, first-boot checks, and captured
install notes.

## Configuration

`install_sets` are the logical trees exposed to `inst`. Each set contains
ordered `layers`, and each layer is either an SGI image or an extracted
directory:

```yaml
install_sets:
  - name: "6.5.30"
    layers:
      - name: overlays1
        image: /media/irix/overlays1.image
        boot: true
      - name: overlays2
        image: /media/irix/overlays2.image
```

Use `dir:` instead of `image:` for an extracted tree. `dist:` maps a media
directory such as `dist6.5` into the canonical `/<set>/dist` path. `boot: true`
marks the one layer whose `stand/` files are served to the PROM. `collisions`
records an explicit winner when two layers contain different bytes at the
same logical path. Identical duplicates are accepted.

The complete example also shows client filtering, service toggles, and the
low TFTP transfer-port range required by SGI PROMs.

## Development

Run the test suite with:

```sh
go test ./...
```

The tests cover the EFS reader, merged virtual trees, collision handling,
generated `inst` commands, BOOTP, TFTP, rsh, and capture behavior. Tests that
need local IRIX media skip when that media is unavailable:

```sh
go test -run RealMedia -v ./internal/vfs ./internal/instcmd
```

Captures retain request, transfer, and timing data from real runs. They make
it possible to compare a later install with a known-good one and turn useful
hardware observations into small synthetic regressions.

## License

MIT
