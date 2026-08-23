# IRIX network install

This is the operator guide for the tested install configuration served by
instigator.
The server does not serve a generated runbook. It serves only the machine
inputs (`/install.cmds` and the logical install-set trees); keep this guide
with the checkout used to start the server.

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

For a dry inspection, type the commands from the file manually and omit the
final `go`. Otherwise the `admin source` command above is the one-shot install.

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
