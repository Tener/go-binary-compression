# Investigation notes — Go binary compression

Write-up of findings from a deep-dive into how Go binaries compress, what
transforms make them compress better, and what build-time knobs reduce the
raw size before compression even enters the picture. Motivated by a
Teleport embed-binary PR, but most of this applies to any Go release
binary.

## Summary of results (reference helper, stripped Linux amd64)

Starting point: a `session/cmd/sessionhelper` Go binary built with
`-trimpath -ldflags="-s -w -extldflags=-s"`.

| Method                                    |      Bytes |   % raw |
|-------------------------------------------|-----------:|--------:|
| raw stripped helper                       |  9,328,008 |  100.0  |
| gzip -9                                   |  3,992,146 |   42.8  |
| xform (BCJ + pcln) + gzip -9              |  3,772,388 |   40.4  |
| zstd -22 --ultra                          |  3,565,312 |   38.2  |
| xform + zstd -22 --ultra                  |  3,243,773 |   34.8  |
| xz -9e                                    |  3,297,036 |   35.3  |
| xz --x86 -9e (LZMA-SDK BCJ + LZMA2)       |  3,171,804 |   34.0  |
| zpaq -m5                                  |  2,800,429 |   30.0  |
| **xform + zpaq -m5**                      |  **2,661,720** |  **28.5**  |

The xform consistently subtracts ~5–6% from whatever the downstream
compressor achieves. On the main Teleport binary (520 MB stripped) the
win grows to **~7.9%** because function count scales linearly with the
biggest components of the transform (ftab Δ-varint).

## Teleport main binary findings

All numbers for `go build -o teleport ./e/tool/teleport` built cross
with `zig cc --target=x86_64-linux-gnu`.

| Build toolchain / flags                              |       Raw |   gzip -9 | zstd -19 |
|------------------------------------------------------|----------:|----------:|---------:|
| zig cc, `-ldflags="-s -w"`                           |  710.5 MB | 153.4 MB  | 106.1 MB |
| zig cc, + `-extldflags=-s`                           |  546.4 MB | 132.2 MB  |  96.9 MB |
| + our xform (BCJ + pcln)                             |  543.6 MB | 121.7 MB  |  89.3 MB |
| GNU gcc (messense) baseline                          |  539.9 MB | 140.5 MB  | 100.8 MB |
| GNU gcc + `-Wl,--no-export-dynamic`                  |  434.6 MB | 103.0 MB  |  72.3 MB |
| GNU gcc + `--no-export-dynamic` + `--hash-style=gnu` |  434.6 MB | 103.0 MB  |  72.3 MB |
| **GNU gcc + `--no-export-dynamic` + our xform**      |**431.8 MB**|**91.8 MB**|**64.0 MB**|

**Compressed size journey: 106.1 → 64.0 MB zstd, −42 MB / −39.6%**.

The three compounding wins, ranked by zstd delta:

1. **`--no-export-dynamic` (GNU gcc)**: **−25 MB zstd** (97 → 72 MB).
   Overrides Go's default `-rdynamic`, killing ~90 MB of `.dynsym`/
   `.dynstr` Go-function entries that nothing at runtime reads.
   Requires GNU ld — lld rejects the flag.
2. **`-extldflags=-s`** (either toolchain): **−9 MB zstd** (106 → 97 MB).
   Strips the classic ELF `.symtab`/`.strtab` that Go's `-ldflags=-s`
   leaves behind.
3. **Our xform (BCJ + pcln)**: **−8 MB zstd** (72 → 64 MB), ~11% off
   the post-strip compressed size.

`--hash-style=gnu` is subsumed: once `--no-export-dynamic` kills the
Go-function entries, `.hash` (proportional to `.dynsym`) shrinks to
basically nothing on its own. The flag saves 9 MB standalone but only
bytes when stacked after `--no-export-dynamic`.

## Key findings

### 1. `-s -w` does NOT strip the classic ELF `.symtab`/`.strtab`

Go's `-ldflags=-s -w` removes Go's own symbol/debug structures and DWARF,
but leaves the classic ELF `.symtab`/`.strtab` in place. On the helper
those are 374 KB + 567 KB = 941 KB of dead weight. On the main Teleport
binary they represent the ~164 MB delta above.

Correct incantation for a fully stripped Go binary is:

```sh
go build -trimpath -ldflags="-s -w -extldflags=-s" -o out ./...
```

- `-s` — Go-level symbol table
- `-w` — DWARF
- `-extldflags=-s` — passes `-s` to the external linker (gcc/zig cc/ld),
  which strips the ELF `.symtab`/`.strtab`

Equivalent to running a post-build `strip` but more reliable (no risk of
missing it in CI, no extra tool dependency, works with any linker
backend).

### 2. `.gopclntab` is not debug info; it's required runtime data

~33% of a typical stripped Go binary. It maps PC values to function
identity, source location, stack-pointer deltas, GC pointer maps, and
defer/recover trampolines. Used by:

- Panic stack traces (why Go panics are still symbolicated after strip)
- GC (pointer bitmaps at every safepoint)
- Stack growth (finds stack-resident pointers to relocate)
- `defer` / `recover`
- `runtime.Callers` / `FuncForPC`
- Profilers (pprof, perf, exec tracer)

Unlike `.symtab`, you cannot drop or substantially modify it without
breaking the binary. Any transform has to be fully reversible before
execution.

### 3. BCJ gives ~2.8% universally on x86-64 Go binaries

The x86 Branch/Call/Jump filter rewrites every `E8`/`E9` opcode's 4-byte
operand from PC-relative displacement to absolute-in-buffer offset.
Identical call targets produce identical byte sequences regardless of
call-site address, so LZ matchers can find them.

Measured impact on the reference helper (9.33 MB stripped):

| Downstream    | Raw       | + BCJ     |  Δ   |
|---------------|----------:|----------:|-----:|
| gzip -9       | 3,992,146 | 3,879,782 | −2.8%|
| zstd -22 ultra| 3,565,312 | 3,464,565 | −2.8%|
| xz -9e        | 3,297,036 | 3,204,688 | −2.8%|

The ratio is striking: same ~2.8% off whether the downstream is gzip,
zstd, or xz. BCJ is operating on the same underlying redundancy that
each compressor sees but can't fully exploit in raw form.

The "always convert" variant (no top-byte range check) is trivially
reversible — `(x + pos + 5) - (pos + 5) = x` for any byte, regardless
of whether the E8/E9 it followed was real code or a data byte that
happened to look like an opcode. Costs ~30 KB of extra compression on
data sections vs. the LZMA-SDK range-checked variant, for a much
simpler implementation.

### 4. `.gopclntab` decomposes into four sub-streams with very different
compressibility

| Sub-table  | % of pcln | zstd ratio |
|------------|----------:|-----------:|
| funcnameTab|     15.4% |    **14%** (LZ nirvana — repeated prefixes) |
| pctab      |     39.3% |    **62%** (varint-packed, defeats LZ) |
| pclnTab    |     43.8% |    **28%** (regular _func records) |
| cu + file  |      1.4% |    ~25%    |

- funcnameTab already compresses near entropy; no easy win.
- pctab is the worst offender. Its contents (delta-varint pairs of
  val_delta/pc_delta) have high byte-level entropy because of the
  bit-packing. Splitting the stream into parallel val and pc bytes
  gives zstd ~50 KB more matches (from 826 KB → 770 KB, −7%).
- The pclnTab's ftab — an array of (entryoff, funcoff) records —
  Δ-varints from ~84 KB → 9.7 KB compressed (−73%). Both columns are
  monotonically increasing.

These are the two transforms the `pcln` package applies.

### 5. Section layout: contiguous, no interleaving

File-offset layout for a typical stripped Go binary is:

    [ELF header, dynamic linking] — [.rodata] — [.gopclntab] — [.typelink/.itablink/eh_frame] — [.text] — [.data/.noptrdata/etc.]

with tiny (3–52 byte) alignment gaps between sections and no interleaving.
So a "handful of ranges with different transforms" pipeline is
straightforward: BCJ on `.text`, pcln xform on `.gopclntab`, everything
else raw. That's exactly what `pipeline.Encode` does.

### 6. Dynamic-linking sections dominate big CGO binaries

Teleport's main binary (546 MB stripped) section breakdown:

    .text        204.7 MB (37.5%)  ← BCJ target
    .gopclntab   151.8 MB (27.8%)  ← pcln xform target
    .rodata       70.3 MB (12.9%)
    .dynstr       65.7 MB (12.0%)  ← CGO bloat
    .dynsym       27.1 MB  (5.0%)  ← CGO bloat
    .hash          9.0 MB  (1.7%)  ← legacy sysv hash, redundant
    .gnu.hash      7.7 MB  (1.4%)
    others         ~10 MB   <2%

**~21% of the binary is dynamic linking bookkeeping.** Not normal for
Go; caused by CGO statically linking C libraries (modernc.org/sqlite
port and similar) and re-exporting every public C symbol into `.dynsym`.

### 7. Linker flag compatibility — lld vs GNU ld

With `CC="zig cc --target=x86_64-linux-gnu"` (lld backend):

| Flag                         | lld? |   Effect                     |
|------------------------------|:---:|-------------------------------|
| `--hash-style=gnu`           |  ✓  | −9.0 MB standalone            |
| `--gc-sections`              |  ign| needs compile-side `-ffunction-sections -fdata-sections` |
| `-Bsymbolic`                 |  ign| no observable effect on Go binaries |
| `-Bsymbolic-functions`       |  ign| "                            |
| `--as-needed`                |  ign| teleport already uses this in CGO_LDFLAGS |
| `--exclude-libs=ALL`         |  ✗  | **rejected** — lld does not implement |
| `--no-export-dynamic`        |  ✗  | **rejected** — lld does not implement |
| `--version-script=FILE`      |  ✓  | achieves same effect; not yet tested |

With `CC=x86_64-unknown-linux-gnu-gcc` (GNU ld / bfd backend,
installed via `brew install messense/macos-cross-toolchains/x86_64-unknown-linux-gnu`):

| Flag                         | GNU? | Effect on teleport           |
|------------------------------|:---:|-------------------------------|
| `--hash-style=gnu`           |  ✓  | −9.0 MB (drops `.hash`)       |
| `--exclude-libs=ALL`         |  ✓  | no-op — CGO deps are shared libs, not static archives |
| `--gc-sections`              |  ✓  | no observable change without compile-side prep |
| `-Bsymbolic-functions`       |  ✓  | trivial delta (0-5 bytes compressed) |
| **`--no-export-dynamic`**    |  ✓  | **−105 MB raw / −25 MB zstd** — the big one |

Most flags that have real effect on Go binaries require GNU ld. For
macOS hosts, the messense cross toolchain is the cleanest path — one
`brew install`, drop-in `CC=x86_64-unknown-linux-gnu-gcc`.

**Two-toolchain rule**: zig cc produces bytes that compress slightly
*better* per byte than GNU gcc output (different CRT / section layout),
but the compressed-size gap (~5 MB on our 96 MB zstd) is dwarfed by
`--no-export-dynamic`'s 25 MB win. GNU gcc wins overall for
CGO-heavy binaries; zig cc remains the better pick when the binary has
minimal CGO.

### 7a. What `.dynsym`/`.dynstr` actually do

Every dynamically-linked ELF has:

- `.dynsym` — fixed 24-byte-per-entry array with info about each
  imported/exported symbol (name offset into .dynstr, type, binding,
  visibility, section index, address, size).
- `.dynstr` — concatenated null-terminated strings, one per symbol
  name, referenced by `.dynsym[i].name_offset`.

The dynamic linker (`ld-linux-x86-64.so.2`) uses these at load time:
for every `shndx=UNDEF` import entry, look up the same name in the
NEEDED shared libraries' `.dynsym`/`.gnu.hash` and fill in the
resolved address. Exports exist for the benefit of *other*
dynamically-linked consumers (none in our case, since teleport
isn't a library), `LD_PRELOAD` interposers, and external debuggers.

Go's linker passes `-rdynamic` (= `-Wl,--export-dynamic`) by default,
which forces **every** global symbol into `.dynsym` — all ~160k Go
functions, their 58-byte average fully-qualified names concatenated
in `.dynstr`. Total: ~93 MB. At runtime none of this is consulted —
Go's own panic/profiler symbolication uses `.gopclntab`, not `.dynsym`.

`--no-export-dynamic` overrides `-rdynamic`, keeping only genuine
imports (libc functions actually referenced). The few hundred kept
entries are all the dynamic linker needs.

**Runtime consequences**: Go panics, `runtime.Callers`,
`debug.PrintStack` still work (pclntab-based). External symbolication
via ELF symbols degrades — `perf`, `gdb` (already poor on Go),
ELF-symbol-based eBPF uprobes, `LD_PRELOAD` interposition of Go
functions all lose their hook points. For production release builds
these are usually acceptable.

### 8. Cascade-shift defeats binary delta schemes on Go binaries

Tested `zstd --patch-from` between two teleport binaries that differ
only by one embedded payload (~10 MB content diff). Patch size: ~65 MB
— 6.5× larger than the actual content difference. Cause: adding content
to `.rodata` shifts every subsequent section, which relocates every
`pclntab` PC offset and every PC-relative CALL/JMP in `.text`. The
shared Go runtime code is semantically identical but byte-level
identical only within basic-block granularity.

Implication: *no* "delta against a reference binary" scheme (patch-from,
bsdiff, trained dictionary against other teleport binaries, etc.) is
going to substantially help compressing a Go binary. Content-defined
chunking + dedup (casync, zchunk, desync) would be better for OTA
delta-updates if anyone ever wants them.

Verified by self-patch test: `zstd --patch-from=X X` produces 82 KB of
pure framing overhead, confirming that when bytes actually match, zstd
can express it in near-zero space. So the 65 MB is genuinely "shifted
bytes that are semantically equivalent but lexically different," not a
zstd limitation.

### 9. Compression method rankings (single file, reference helper)

| Method               | Bytes | Ratio |  Compress | Decompress | Decompress RSS |
|----------------------|------:|------:|----------:|-----------:|---------------:|
| gzip -9              | 4.20 MB | 40.7% | 0.9 s | instant | ~2 MB |
| zstd -1              | 4.62 MB | 44.7% | 0.01 s | instant | 5 MB |
| zstd -22 --ultra     | 3.57 MB | 34.5% | 1.6 s | instant | 14 MB |
| xz -9e               | 3.30 MB | 31.9% | 2.1 s | 0.01 s | few MB |
| xz --x86 -9e         | 3.17 MB | 30.7% | 1.9 s | 0.01 s | few MB |
| zpaq -m5             | 2.90 MB | 28.0% | ~20 s | few s | tens of MB |
| cmix                 | ? | ? | hours | ? | 10+ GB |

Decompression memory is essentially free for every method except cmix
(which is impractical for runtime use anyway).

For the teleport-helper embed case specifically, gzip is the right
runtime decompressor despite being the largest: its pure-Go
`compress/gzip` implementation avoids a CGO decompression dependency
and the size difference vs zstd at startup-time is a few MB on a
binary that's already 500 MB.

### 10. Trainable dictionaries: diminishing returns for a single large file

zstd dictionaries are designed for many small samples that share
structure. A single 10 MB Go binary already contains most of its
redundancy inside its own LZ window. A dictionary trained on other Go
binaries (or past helper builds) captures ~100 KB of stable runtime
constants that zstd already finds in the first ~1% of the input.
Estimated realized savings: 100-300 KB gross, net after shipping the
~100 KB dictionary: 0-200 KB. Not worth the pipeline complexity.

### 11. BCJ as a standalone tool is rare; write your own

BCJ is bundled inside xz, 7z, LZMA-SDK — not exposed as a standalone
CLI. The transform is ~30 lines in any language (scan for E8/E9,
rewrite 4 operand bytes, advance by 5). Writing a simple Go version
and verifying round-trip on random data + a real binary is a half-hour
of work. See `bcj/bcj.go` in this repo.

## Recommended full recipe for a CGO-heavy Go release binary

For a Linux amd64 Go binary with significant CGO dependencies,
targeting the smallest possible compressed release artifact:

```sh
# Prereqs on macOS:
#   brew install messense/macos-cross-toolchains/x86_64-unknown-linux-gnu
#   brew install zstd  # (or the compressor of your choice)

CC=x86_64-unknown-linux-gnu-gcc \
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 \
  go build -trimpath -buildvcs=false \
    -ldflags='-s -w -extldflags "-Wl,--strip-all -Wl,--no-export-dynamic"' \
    -o mybinary ./...

go run github.com/Tener/go-binary-compression/cmd/gobc@latest \
  encode mybinary mybinary.xform

zstd -19 --long=30 -T0 mybinary.xform -o mybinary.xform.zst
```

Consumer side:

```sh
zstd -d --long=30 mybinary.xform.zst -o mybinary.xform
go run github.com/Tener/go-binary-compression/cmd/gobc@latest \
  decode mybinary.xform mybinary
```

On the reference Teleport binary this takes the zstd-compressed
shipping artifact from 106 MB → 64 MB.

## Open questions / future directions

- `-Wl,--version-script` with an empty export list, tested with lld —
  would unlock `--no-export-dynamic`-equivalent behavior on zig cc.
  Not yet tried.
- Rewriting the helper in C to shrink from ~10 MB Go binary to
  ~100 KB C binary (at the cost of duplicating all reexec subcommand
  logic). Removes the need for compression at all. Large engineering
  scope.
- Better pcln transforms:
  - Parse pctab into per-function tables with proper boundary
    detection (my state-machine-based byte split is approximate but
    reversible; a proper parser could enable per-table re-encoding)
  - Dictionary coding of funcname prefixes (potential: ~100 KB gross,
    much less net after dictionary overhead)
- BCJ variants for ARM64 (Teleport needs arm64 helper too) and ARM.
  Fixed-width 32-bit instructions, similar mask+rewrite pattern on
  `BL`/`B` immediate fields.
- Apply `--no-export-dynamic` to tsh, tctl, tbot — same fix applies
  to every Go binary built with CGO on Linux; expected proportional
  wins on each.

## References

- LZMA SDK's x86 BCJ: `Bcj.c` in 7-Zip source (Igor Pavlov, public domain)
- Go pclntab format: `src/runtime/symtab.go`, `src/cmd/link/internal/ld/pcln.go`
- zstd `--patch-from`: https://github.com/facebook/zstd/releases/tag/v1.4.5
- Google Chrome's BsDiff / Courgette: related class of tools that
  address the same cascade-shift problem for browser updates
- xz BCJ filters: `src/liblzma/simple/*.c` in xz-utils
