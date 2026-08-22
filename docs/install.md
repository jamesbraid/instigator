# IRIX network install

This is the operator guide for the install profiles served by instigator.
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
choice for that profile, and starts the install. It does not use positional
`conflicts` choices.

For a dry inspection, type the commands from the file manually and omit the
final `go`. Otherwise the `admin source` command above is the one-shot install.

## Proven profile ordering

For the current `6.5.30` base profile, keep the enabled sets in this order:

1. `6.5.30` overlays, with all overlay discs merged into one set.
2. `foundations`, with Foundation and NFS media merged together.
3. `development`, containing Development Foundation/Libraries.
4. `applications`.
5. `complementary`.
6. `freeware`, when enabled.

The generated command file opens the supplemental sets in that order and
reopens `6.5.30` last. That ordering is based on the successful Octane run and
is part of the profile contract.

## Profiles

Profiles are configuration selections, not separate server modes. The first
enabled set is the primary release and later enabled sets are opened in the
configured order.

- `6.5.30`: the six-set ordering above.
- `6.5.30 + compilers`: the `6.5.30` profile plus a later MIPSpro/compiler
  set. Keep this as a separate profile until the base profile is proven.
- `6.5.22`: an older primary release and its matching supplemental media for
  hardware that cannot boot newer IRIX releases.

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
