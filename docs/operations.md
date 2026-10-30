# Running and troubleshooting the server

Use the [installation guide](install.md) for your first install and the
[configuration guide](configuration.md) for media and script settings.

## Network setup

On the host network, the server binds privileged ports (67 for BOOTP, 69 for
TFTP), so start it with `sudo` on Linux and macOS, or as Administrator on
Windows. On Windows, also allow UDP 67/69, the configured TFTP transfer-port
range, and TCP 514 through the firewall, or the PROM's requests never reach
it.

The BOOTP reply is broadcast, and the OS routing table picks the interface it
leaves by. On a multi-homed host the reply can exit the wrong interface and
the SGI never sees it. Run the server where the install network is the active
route, or disable the other interfaces while installing.

Capture directories rely on Unix permission bits (0700/0600) to stay private.
On Windows, choose a capture directory protected by the filesystem's access
controls. Unix mode bits do not provide that protection.

## Emulator connections

`serve --network-tcp IP:PORT` listens for a QEMU stream on a loopback TCP
address. `--network-socket PATH` uses a Unix socket instead. These modes serve
the guest's private Ethernet segment without binding install ports on the
host, so they need no `sudo`. Start Instigator first, then connect the
emulator. Use one option at a time.

The config's `server_ip`, `netmask`, and client addresses must describe the
private segment. Configured clients get BOOTP and DHCP replies there.
Host-network mode answers BOOTP only. The private segment has no gateway or
DNS. For attachment details, see the [network
reference](nfs-shares.md#private-network).

`serve --share NAME=DIR` also serves writable NFS shares on that segment. See
the [sharing guide](sharing.md#shares-alongside-an-install).

## Install timing

Every generated script uses native `admin source` handoffs to time `go`. Each
execution gets a separate token to identify its start and return. A script
with a second pass records two attempts: `NAME.cmds` for the first `go` and
`NAME.cmds then` for the second.

Timestamped INFO logs identify the script, client, and attempt. Return records
include elapsed time. Capture and `-v` are optional. Example with token
abbreviated:

```text
2026-10-03T12:00:00Z INFO  install_start: install.cmds (o200), attempt <token>: go dispatched
2026-10-03T12:22:50Z INFO  install_returned: install.cmds (o200), attempt <token>: go returned after 22m50.81s (success not verified)
```

To retain the timing events and transfer statistics, give each server run a
fresh capture directory:

```sh
sudo ./instigator serve --capture-dir RUN instigator.yaml
```

Stop the server with Ctrl-C, then summarize the capture. Use the same
privileges as the server so the summary command can read the private files:

```sh
sudo ./instigator trace summary RUN
```

`events.jsonl` retains these events. The `installs` array in `summary.json`
adds timestamps and `duration_ms` to each attempt. Missing return markers
leave attempts `incomplete` with no finished duration. Regenerate the summary
after a crash with `trace summary`.

Timing includes marker fetch overhead and prompts during `go`. Earlier
commands and reboot are outside the interval. A return means execution reached
the command after `go`. Verify success from guest output and first boot.
Errors that abort the command file leave the attempt incomplete. Handwritten
scripts and commands entered at `Inst>` have no markers.

Chaining and the newline-only marker were tested with installed IRIX 6.5
`inst` 4.1. A full successful miniroot install with these markers remains
untested.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `check` passes but `serve` rejects media | Remote checks probe URLs by default. Run `check --full` to read the media and check collisions and products. |
| PROM waits for a BOOTP reply | Match the configured MAC to `printenv eaddr`, check UDP 67, and check which interface carries broadcast replies. |
| TFTP starts but stalls | Allow the configured transfer-port range as well as UDP 69. SGI PROMs need low transfer ports. |
| Installer waits at `Install software from` | Enter the number beside `done` to resume the command file. |
| Add-ons were not installed | Source `/irix.cmds` with shipped configs. The baseline `/install.cmds` has no add-ons. |
| NFS mount cannot reach the server | Check the listen address and portmap. See the sharing guide for host and private-network ports. |

`serve --verbose` decodes packets. `share --verbose` logs RPC calls. Capture a
fresh install run when you need transfer and timing records.

## Private network disconnects

With `--network-socket` or `--network-tcp`, an emulator network disconnect
stops Instigator with a timestamped error and exit status 1. For example,
`private network disconnected: read frame: EOF` reports the connection
closing. It does not establish installation completion. Capture ends with
`server_stop` / `disconnected`. Attempts without return markers stay
incomplete.

## Running it as a service

On the host network, `instigator serve` reports readiness through `sd_notify`
after assembling the install sets and binding the listeners. A `Type=notify`
unit waits for that signal before `systemctl start` returns.

With `--network-socket` or `--network-tcp`, readiness means the private
network listener is waiting for the emulator. Media assembly and serving begin
after the emulator connects. Run `instigator check` before starting the
service if you need to validate the media first.

Install the binary at `/usr/local/bin/instigator` and the config at
`/etc/instigator.yaml`, then install this unit as
`/etc/systemd/system/instigator.service`:

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
sudo systemd-notify --fork -- /usr/local/bin/instigator serve /etc/instigator.yaml
```

It returns 0 once the server is serving and leaves it running, or non-zero
with the server's own error.

On macOS, use a launchd plist with `ProgramArguments` set to the binary,
`serve`, and the configuration path. launchd has no readiness protocol, so
`launchctl` reports the job started once the process runs, not once it is
serving.
