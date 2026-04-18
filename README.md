# go-binary-compression

Format-aware preprocessing transforms that make Go ELF binaries compress
better. Two transforms, both losslessly reversible:

- **BCJ** — an x86-64 Branch/Call/Jump filter that rewrites PC-relative CALL
  and JMP displacements as absolute offsets, so identical targets produce
  identical bytes regardless of call-site address.
- **pcln** — a structure-aware transform for Go's `.gopclntab` section that
  splits the packed `pctab` varint stream into parallel val/pc streams,
  delta-encodes the `ftab` entryoff/funcoff columns, and column-splits
  every `_func` record into per-field streams (entryOff, nameOff, args,
  pcsp, pcfile, pcln, npcdata, cuOffset, …) with delta-varint encoding on
  the offset-like columns.

The combined pipeline is applied only to the `.text` and `.gopclntab`
sections; everything else passes through unchanged. The output is a
self-describing byte blob with a small envelope that lets `Decode` reverse
the transform byte-for-byte.

## Why preprocess at all?

Downstream compressors (gzip, zstd, xz, zpaq) all work on raw bytes. They
see `E8 FB EF 04 00` at one call site and `E8 FB DF 04 00` at another —
the same semantic call to `runtime.newobject`, but the byte patterns differ
because the PC-relative displacement encoding embeds the call-site address.
BCJ canonicalizes those patterns. Similarly, Go's `pctab` is a dense
varint stream whose bit-packing defeats LZ matchers; splitting it into
homogeneous sub-streams exposes the real redundancy.

## Measured results

Four Linux amd64 Go ELFs, all built with `-trimpath -ldflags="-s -w"`
(plus the usual `--strip-all,--no-export-dynamic` extldflags). `crdb`
retains DWARF debug info; the other three are fully stripped.

- `helper`         — `testdata/helper_linux_amd64` (small reference, 9.3 MB)
- `crdb.stripped`  — CockroachDB, stripped (206 MB)
- `crdb`           — CockroachDB, with DWARF left in (290 MB)
- `teleport`       — Gravitational Teleport OSS (414 MB)

### Transform only (no downstream compressor)

The encode step is nearly length-preserving. BCJ rewrites call/jump
displacements in place, and the pclntab column-split + signed-delta
varints end up marginally denser than the original packed encoding — a
small free win before any compressor runs.

| Binary             |         Raw |     Encoded |   Δ bytes |    Δ %  |
|--------------------|------------:|------------:|----------:|--------:|
| `helper`           |   9,328,008 |   9,129,319 |  −198,689 |  −2.13% |
| `crdb.stripped`    | 216,195,264 | 212,154,990 |−4,040,274 |  −1.87% |
| `crdb` (w/ DWARF)  | 304,645,008 | 300,604,734 |−4,040,274 |  −1.33% |
| `teleport`         | 434,615,632 | 425,640,664 |−8,974,968 |  −2.07% |

### gzip -9

| Binary             |      Raw→gz |     xform→gz |    Δ bytes |    Δ %  |
|--------------------|------------:|-------------:|-----------:|--------:|
| `helper`           |   3,992,153 |    3,686,822 |   −305,331 |  −7.65% |
| `crdb.stripped`    |  67,337,696 |   60,692,090 | −6,645,606 |  −9.87% |
| `crdb` (w/ DWARF)  | 125,821,777 |  119,174,842 | −6,646,935 |  −5.28% |
| `teleport`         | 103,004,190 |   87,943,001 |−15,061,189 | −14.62% |

### zstd --long=30 -19

| Binary             |     Raw→zst |    xform→zst |    Δ bytes |    Δ %  |
|--------------------|------------:|-------------:|-----------:|--------:|
| `helper`           |   3,444,362 |    3,190,444 |   −253,918 |  −7.37% |
| `crdb.stripped`    |  52,725,387 |   47,324,875 | −5,400,512 | −10.24% |
| `crdb` (w/ DWARF)  | 109,233,408 |  103,827,258 | −5,406,150 |  −4.95% |
| `teleport`         |  72,288,243 |   61,614,025 |−10,674,218 | −14.77% |

The DWARF-carrying `crdb` column shows the expected dilution: the debug
sections pass through unchanged, so the savings are diluted across a
larger total. `teleport` gets the biggest gain because it has an
exceptionally large `.gopclntab` — thousands of functions whose `_func`
records share strong columnar patterns that the column-split + delta
encoding exposes to the downstream matcher.

See `internal/bench/bench_test.go` for a reproducible matrix (gzip, xz,
zstd, zpaq) on the small reference binary.

## Usage

### Library

```go
import "github.com/Tener/go-binary-compression/pipeline"

blob, err := pipeline.Encode(elfBytes)
// compress blob with gzip/zstd/xz/zpaq, ship it…
// …receiver decompresses to `blob` again
recovered, err := pipeline.Decode(blob)
// recovered is byte-identical to the original elfBytes
```

### CLI

```sh
go build -o gobc ./cmd/gobc

./gobc encode     helper      helper.xform
./gobc roundtrip  helper         # verifies encode→decode is byte-identical
gzip -9 < helper.xform > helper.xform.gz
# …later:
gzip -d < helper.xform.gz | ./gobc decode /dev/stdin restored
cmp helper restored              # confirms lossless round-trip
```

## Constraints and assumptions

- **x86-64 Linux ELF** only. The BCJ filter is x86-specific; other arches
  would need their own variants (ARM64 `BL`/`B` immediate rewriting would
  be the analogous transform).
- **Go 1.18+ pclntab layout** (magic `0xFFFFFFF1`). Older binaries are
  rejected — add a new handler in `pcln/` if you need to support them.
- **ftab funcoffs must be monotonic.** The `_func` records must be laid
  out in the same order as ftab entries (this is true for every Go linker
  output we've seen); the encoder errors otherwise.
- The BCJ filter is the "always-convert" variant (no LZMA-SDK top-byte
  range check). It's trivially reversible and gives most of the win; the
  range-checked variant would gain another ~30 KB of compression at the
  cost of buffer-size-dependent reversibility edge cases.

## Package layout

    bcj/                 x86 BCJ encode/decode (public)
    pcln/                .gopclntab encode/decode (public, with Meta side-info)
    pipeline/            ELF-aware wrapper combining both (public)
    cmd/gobc/            CLI entry point
    internal/bench/      compression-matrix test across gzip/xz/zstd/zpaq
    testdata/
      helper_linux_amd64 reference binary (stripped Go binary, 9.3 MB)

## Tests

```sh
go test ./...                       # round-trip tests (all packages)
go test -v ./internal/bench         # compression matrix — requires gzip, xz,
                                    # zstd, zpaq on PATH (each is skipped if
                                    # absent)
go test -bench=. ./bcj ./pipeline   # microbenchmarks
```

## Non-goals

- Not a general-purpose compressor — it only pre-processes; the downstream
  compressor is still responsible for the compression itself.
- Not a runtime loader. The transformed blob is not a runnable ELF; you
  must `Decode` it before the kernel can execute the bytes.
- Not a full replacement for LZMA-SDK BCJ. If you already use xz's `--x86`
  filter, this library saves ~30 KB and adds complexity; use xz's filter
  directly. The reason to build on this pipeline is to get BCJ + pcln
  combined with *any* downstream compressor (e.g., pure-Go `compress/gzip`
  or `klauspost/compress/zstd`).
