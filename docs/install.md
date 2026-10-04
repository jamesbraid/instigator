# IRIX network install

This is the operator guide for the tested install configuration. Keep it with
the checkout used to start the server.

## Server host setup

The server binds privileged ports (67 for BOOTP, 69 for TFTP), so start it with
`sudo` on Linux and macOS, or as Administrator on Windows. On Windows, also
allow UDP 67/69 and TCP 514 through the firewall, or the PROM's requests never
reach it.

The BOOTP reply is broadcast, and the OS routing table picks the interface it
leaves by. On a multi-homed host the reply can exit the wrong interface and the
SGI never sees it. Run the server where the install network is the active route,
or disable the other interfaces while installing.

Capture directories rely on Unix permission bits (0700/0600) to stay private.
On Windows those bits are near no-ops, so point `--capture-dir` somewhere the
filesystem already protects.

## Boot the miniroot

At the Octane PROM command monitor:

```text
setenv netaddr <client-ip>
boot -f bootp():/<primary-set>/stand/fx.64
```

At the `Remote Directory` prompt, enter the primary distribution without a
trailing slash:

```text
/<primary-set>/dist
```

## Load the install selections

At `Inst>` load the command file from the server:

```text
admin source <server-ip>:/install.cmds
```

The command file opens every enabled supplemental set and reopens the primary
release last. If `inst` opens an `Install software from` menu, enter the number
beside `done`. The command file then selects the standard product set, applies
the known package choice for this install, and starts the install. It does not
use positional `conflicts` choices.

For a dry run, type the selection commands from the file manually and omit the
final `admin source` command, which starts `go`.

### Named install scripts

If the configuration defines `install_scripts`, each one is served next to the
default at `/<name>.cmds`. Load a named script instead of the default to get its
extra selections — for example a `debug` script that also installs `dbx`:

```text
admin source <server-ip>:/debug.cmds
```

The default `/install.cmds` remains available, so choose a script by sourcing
its path. The named script opens the same sets in the same order, then applies
the standard selection plus that script's own
`install`/`keep`/`remove` lines and release stream.

### Install timing

Every generated script ends by fetching a small command file that runs `go`
and then fetches a return marker. Instigator logs the elapsed time between
these fetches. Each execution gets its own token, so repeated installs and
different clients have separate timings.

Normal INFO logs include timestamped `install_start` and `install_returned`
records. Both identify the script, client, and attempt. The return record also
reports elapsed time. These records are emitted even without `--capture-dir`
or `-v`. For example, with the attempt token abbreviated:

```text
2026-10-03T12:00:00Z INFO  install_start: install.cmds (o200), attempt <token>: go dispatched
2026-10-03T12:22:50Z INFO  install_returned: install.cmds (o200), attempt <token>: go returned after 22m50.81s (success not verified)
```

To retain the timing events and transfer statistics, give each server run a
fresh capture directory:

```sh
instigator serve --capture-dir RUN config.yaml
instigator trace summary RUN
```

`events.jsonl` records `install_start` and `install_returned`. The `installs`
array in `summary.json` identifies the script, client, attempt, timestamps,
and `duration_ms`. An attempt without a return marker is `incomplete` and has
no finished duration. The summary can be regenerated after a crash.

This measures the `go` operation, including the marker fetch overhead and any
menus or prompts encountered during `go`. Commands before `go` and reboot
are outside that interval. A return marker means execution reached the command
after `go`. Inspect the guest output and first boot to establish installation
success. If an error aborts the command file, the attempt stays incomplete.
Handwritten scripts and commands entered directly at `Inst>` have no timing
markers.

The native command-file handoff and newline-only return marker were tested
with installed IRIX 6.5 `inst` 4.1. Cancellation and an empty selection aborted
the command file before its return marker. A full successful miniroot install
with these markers has not been tested.

### Private network disconnects

With `--network-socket` or `--network-tcp`, closing the emulator's network
connection stops Instigator. A message such as `private network disconnected:
read frame: EOF` reports that connection closing. It does not establish
installation completion. The final error includes a timestamp and exits with
status 1.

With capture enabled, Instigator finalizes the run with a `server_stop` event
whose result is `disconnected`. Attempts without return markers remain
`incomplete`.

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
discs within a set are merged into one logical `/<set>/dist` tree; disc names
are never exposed to `inst`.

## First-boot checks

After installation, use the restart prompt's shell before rebooting if you
want to inspect the target root mounted at `/root`:

```sh
ls -l /root/unix /root/usr/stand/ide
dvhtool -v list /dev/rdsk/dks0d1vh
```

The volume header should contain `sash`, `ide`, and the machine PROM. The
installed root should contain `/unix`. The PROM normally loads `sash` from the
volume header, not from `/stand` in the root filesystem.

## Running it as a service

Once the install sets are assembled and the listeners are bound,
`instigator serve` reports itself ready over `sd_notify`. A `Type=notify`
unit's `systemctl start` therefore returns when the server is serving
rather than when the process spawned, which is what a caller wants when it
is about to boot a machine at it.

Install this unit as `/etc/systemd/system/instigator.service`:

```ini
[Unit]
Description=instigator IRIX install server
After=network-online.target

[Service]
Type=notify
# Assembling the sets reads each image over the network; allow for a slow
# store rather than systemd's default start timeout.
TimeoutStartSec=600
ExecStart=/usr/local/bin/instigator serve /etc/instigator.yaml
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

To start the server from a script and wait until it is serving:

```sh
systemd-notify --fork -- instigator serve /etc/instigator.yaml
```

It returns 0 once the server is serving and leaves it running, or non-zero
with the server's own error.

On macOS the same shape as a launchd plist, with `ProgramArguments` set to
the binary, `serve`, and the configuration path. launchd has no readiness
protocol, so `launchctl` reports the job started once the process runs,
not once it is serving.
