# Extracting installed files

`instigator extract` reads IRIX software distributions and writes the files
they would install to a host directory. It also writes a JSON Lines manifest
with the source, status, and metadata of each entry. Use it to inspect files
or extract manual pages without running an IRIX installation.

For example:

```sh
./instigator extract --path usr/share/catman disc.image out/
```

This writes `out/disc/` and `out/disc.jsonl`. The source image stays
read-only. You can use a [configured install set](configuration.md) when the
files span several discs.

## Sources and install order

The command accepts images or directories directly, or a YAML configuration:

```sh
./instigator extract [--path PATH]... [--subsystem GLOB]... [--unpack | --manifest-only] [--dist DIR] [--name NAME] <image|dir>... <outdir>
./instigator extract [--set NAME]... [--path PATH]... [--subsystem GLOB]... [--unpack | --manifest-only] <config.yaml> <outdir>
```

When you pass images or directories directly, they form the layers of one
install set in the order given. Files at the same path must agree. Use a
configuration file to resolve collisions between layers.

`--dist` names the distribution directory in every source: `dist` by default,
or `.` for the source root, as in an unpacked tardist. `--name` sets the
output name, which defaults to the first source's name without its extension.

A single `.yaml` or `.yml` source is treated as a configuration file. It can
be a full server config or contain only `install_sets:`. `--set` picks sets by
name. By default, all sets are extracted. Sets use the same merge rules as the
server. The extraction-only `after` field names sets to extract first. `serve`
ignores it:

```yaml
install_sets:
  - name: base
    layers: [...]            # the base release's discs
  - name: overlay
    after: [base]            # transitive; unknown names and cycles are refused
    layers: [...]            # the overlay release's discs
```

Extracting `overlay` writes the base sets in order, then the overlay. If a
later set ships the same product, it replaces the whole earlier product. For
other shared install paths, the later set wins. Replaced entries are marked
`superseded` and their data is never read.

If one set installs a directory and a later set installs a file or link at
that path, the later entry is an `error`. Extraction approximates the
installed files. It does not run `inst`. Files that an overlay retires without
reinstalling remain in the output.

## Filters and compression

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
  have status `planned` and no `path`, `sha256` or `target`. Whether one
  would be written, a duplicate, a variant or an error needs its data.
  Filters and `after` sets apply as usual. It cannot be combined with
  `--unpack`.

## Output and manifest

Files keep their `.idb` permission bits plus owner access. Nothing is chowned.
Device, socket and type `X` entries, and `delhist` entries (paths an older
release installed), are recorded but not created. The manifest has one JSON
object per entry, in product then `.idb` order, with empty fields omitted:

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
`<path>.<mach>`, or `<path>.<first 8 hex digits of its sha256>` when it has no
`mach` or that name would exceed 255 bytes. An entry that cannot be extracted,
for example because its archive's disc is not in the set, is an `error`. The
run carries on and exits non-zero. Existing output is never overwritten.
