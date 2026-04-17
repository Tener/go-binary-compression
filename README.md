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

Reference binary: a stripped Linux amd64 Go binary at **9,328,008 bytes**
(`testdata/helper_linux_amd64`). Built with
`go build -trimpath -ldflags="-s -w -extldflags=-s"`.

| Compressor      | Raw        | xform      |    Δ bytes | Δ %    |
|-----------------|-----------:|-----------:|-----------:|-------:|
| gzip -9         | 3,992,146  | 3,686,812  |   −305,334 | −7.6%  |
| zstd -22 ultra  | 3,444,321  | 3,190,457  |   −253,864 | −7.4%  |
| xz -9e          | 3,190,676  | 2,985,692  |   −204,984 | −6.4%  |
| **zpaq -m5**    | **2,800,370** | **2,619,618** | **−180,752** | **−6.5%** |

Consistent 6–8% reduction across all four compressors on top of whatever
they already achieve. See `internal/bench/bench_test.go` to reproduce.

### Scale test: 434 MB real-world Go binary

On a production `teleport` binary (Linux amd64, 434,615,632 bytes raw),
zstd -15 --long=30 compresses the transformed blob to **69,295,511 bytes**
vs. 83,431,864 for the raw input — **a 16.9% reduction** where the
downstream compressor alone only achieves the raw number. The funcRec
column-split is the dominant contributor at that scale: it trims a further
~4.2% beyond the previous transform because `.gopclntab` scales linearly
with function count, and large Go binaries have many thousands of
functions whose record fields share strong columnar patterns.

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
