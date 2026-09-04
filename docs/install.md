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

The command file opens every enabled supplemental set, reopens the primary
release last, selects the standard product set, applies the known package
choice for this install, and starts the install. It does not use positional
`conflicts` choices.

For a dry run, type the commands from the file manually and omit the final `go`.

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

`instigator serve` is the same command in a terminal and under a service
manager: it notices when a manager started it and lets the manager drive
start and stop instead of waiting on signals. Once the install sets are
built and the listeners are bound, it reports ready over `sd_notify`, so a
`Type=notify` unit's `systemctl start` returns when the server is actually
serving rather than when the process spawned.

On systemd, install this unit as `/etc/systemd/system/instigator.service`:

```ini
[Unit]
Description=instigator IRIX install server
After=network-online.target

[Service]
Type=notify
# Readiness waits for the install sets, which fetch their media; that is
# minutes for a full release, well past systemd's default start timeout.
TimeoutStartSec=1800
ExecStart=/usr/local/bin/instigator serve /etc/instigator.yaml
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

On macOS, the same shape as a launchd plist in
`/Library/LaunchDaemons/dev.octanix.instigator.plist`, with
`ProgramArguments` set to the binary, `serve`, and the configuration path.
launchd has no readiness protocol, so `launchctl` reports the job started
once the process is running, not once it is serving.

On Windows there is no equivalent file to write: a background service has
to be registered with the Service Control Manager, so the binary does it
itself.

```console
> instigator install C:\instigator\instigator.yaml
> sc start instigator
> instigator uninstall
```

The service manager's running state means a serving server on Windows too:
the whole startup - configuration, media, listeners - finishes before the
service reports as started, and a failure to start is reported as one.
