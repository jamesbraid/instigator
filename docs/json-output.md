# JSON output

`instigator serve --json` writes status for a program that runs Instigator,
such as an emulator front end. Stdout carries only JSON Lines: one object per
line. The human log stays on stderr. With `--capture-dir`, the capture's
closing summary also moves to stderr.

```sh
./instigator serve --json --network-socket /tmp/sgi.sock instigator.yaml
```

Every line starts with the same three fields:

| Field | Meaning |
| --- | --- |
| `v` | Schema version, `1`. It changes only for an incompatible change. |
| `time` | When the line was written, in RFC 3339 UTC with nanoseconds |
| `type` | One of the types below |

Ignore types and fields you do not know. New ones can appear without a version
change. Keep reading stdout while Instigator runs. If you fall behind, it drops
`progress` lines rather than delay the machine, and other lines wait.

## Types

### `ready`

The first line of a run that starts serving: the media are assembled and
every service is about to answer. With `--network-socket` or `--network-tcp`,
Instigator assembles the media after the emulator connects, so `ready`
follows the connection. Start the emulator once the socket or port accepts
connections.

| Field | Meaning |
| --- | --- |
| `server_ip` | The server address from the config |
| `network` | `host`, `socket` or `tcp` |
| `sets` | The enabled install sets `inst` can open first, add-ons excepted: `name` and `dist`, the path to give `inst`. Empty when none is enabled. |

### `bootp`

One BOOTP or DHCP request.

| Field | Meaning |
| --- | --- |
| `client` | The configured client name. Absent for an unknown machine. |
| `mac` | The machine's MAC address |
| `ip` | The address offered. Absent when ignored. |
| `file` | The boot file requested, or `dhcp` and the message type |
| `result` | `answered`, `ignored` (not a configured client) or `error` |

### `tftp`

A file transfer to the PROM: `phase` is `start`, then `progress` at most once
a second, then `end`. A transfer that fails before sending, such as a missing
file, has only an `end`.

| Field | Meaning |
| --- | --- |
| `client` | The configured client name, or the machine's IP |
| `file` | The name the PROM asked for |
| `phase` | `start`, `progress` or `end` |
| `size` | The file's size in bytes |
| `bytes_sent` | Bytes sent so far |
| `result` | On `end` only: `ok`, `unacked_final`, `client_stopped`, `aborted`, `gaveup`, `notfound` or `error` |

`client_stopped` means the PROM ended the transfer after taking data. It is
normal for `dist/sa`: the PROM reads the part of the archive it needs and
stops. `aborted` means the PROM refused the file before taking any, or
Instigator could not send. `unacked_final` means the PROM did not acknowledge
the last block, which some PROMs never do. A `notfound` for
`dist/unix.IP22` or a similar kernel while loading the IRIX 5.3 miniroot is
the PROM probing before it falls back to the kernel inside `dist/sa`.

### `inst`

The installer at work over rsh.

| Field | Meaning |
| --- | --- |
| `client` | The configured client name |
| `phase` | `session_start`, `session_end` or `go_returned` |
| `result` | On `session_end` only: `ok`, `idle` (closed after the idle timeout) or `error` |

`go_returned` comes from the generated install scripts, so it appears only
when `inst` loaded one with `admin source`. Before IRIX 6.5, `inst` has no
`admin source`, so it never appears. A return does not mean the installation
succeeded: only the console shows that, with `Installations and removals were
successful.`

### `stopped`

Always the last line.

| Field | Meaning |
| --- | --- |
| `reason` | `signal`, `guest_disconnected` (the emulator closed the network) or `error` |
| `error` | With `error`: the message, which is also the last line on stderr |

The exit status matches: 0 for `signal` and `guest_disconnected`, 1 for
`error`.
