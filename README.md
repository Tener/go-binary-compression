# go-binary-compression

Lossless preprocessing for Go ELF binaries that improves downstream
compression.

- **BCJ** — x86-64 Branch/Call/Jump filter. Rewrites PC-relative CALL/JMP
  displacements as absolute offsets so identical call targets produce
  identical bytes.
- **pcln** — `.gopclntab` transform. Splits the packed `pctab` into
  parallel val/pc streams, delta-encodes `ftab`, and column-splits each
  `_func` record into per-field streams with delta-varint on offset
  columns.

Only `.text` and `.gopclntab` are transformed; the rest passes through.
`Decode` reverses the transform byte-for-byte.

## Measured results

Four Linux amd64 Go ELFs:

| Binary            | Size   |                                      |
|-------------------|-------:|--------------------------------------|
| `helper`          |   9 MB | `testdata/helper_linux_amd64`        |
| `crdb.stripped`   | 206 MB | CockroachDB, stripped                |
| `crdb` (w/ DWARF) | 290 MB | CockroachDB with debug info retained |
| `teleport`        | 414 MB | Teleport OSS                         |

### Transform only (no downstream compressor)

Nearly length-preserving — a small free win before any compressor runs.

| Binary            |         Raw |     Encoded |    Δ bytes |    Δ %  |
|-------------------|------------:|------------:|-----------:|--------:|
| `helper`          |   9,328,008 |   9,129,319 |   −198,689 |  −2.13% |
| `crdb.stripped`   | 216,195,264 | 212,154,990 | −4,040,274 |  −1.87% |
| `crdb` (w/ DWARF) | 304,645,008 | 300,604,734 | −4,040,274 |  −1.33% |
| `teleport`        | 434,615,632 | 425,640,664 | −8,974,968 |  −2.07% |

### gzip -9

| Binary            |      Raw→gz |    xform→gz |    Δ bytes |    Δ %  |
|-------------------|------------:|------------:|-----------:|--------:|
| `helper`          |   3,992,153 |   3,686,822 |   −305,331 |  −7.65% |
| `crdb.stripped`   |  67,337,696 |  60,692,090 | −6,645,606 |  −9.87% |
| `crdb` (w/ DWARF) | 125,821,777 | 119,174,842 | −6,646,935 |  −5.28% |
| `teleport`        | 103,004,190 |  87,943,001 |−15,061,189 | −14.62% |

### zstd --long=30 -19

| Binary            |     Raw→zst |   xform→zst |    Δ bytes |    Δ %  |
|-------------------|------------:|------------:|-----------:|--------:|
| `helper`          |   3,444,362 |   3,190,444 |   −253,918 |  −7.37% |
| `crdb.stripped`   |  52,725,387 |  47,324,875 | −5,400,512 | −10.24% |
| `crdb` (w/ DWARF) | 109,233,408 | 103,827,258 | −5,406,150 |  −4.95% |
| `teleport`        |  72,288,243 |  61,614,025 |−10,674,218 | −14.77% |

### zpaq -m5

| Binary            |    Raw→zpaq |  xform→zpaq |    Δ bytes |    Δ %  |
|-------------------|------------:|------------:|-----------:|--------:|
| `helper`          |   2,800,353 |   2,619,613 |   −180,740 |  −6.45% |
| `crdb.stripped`   |  42,009,397 |  39,051,757 | −2,957,640 |  −7.04% |
| `crdb` (w/ DWARF) |  98,349,196 |  95,389,409 | −2,959,787 |  −3.01% |
| `teleport`        |  55,350,874 |  49,213,847 | −6,137,027 | −11.09% |

DWARF dilutes `crdb`'s gain — debug sections pass through untouched.
`teleport` wins hardest: its oversized `.gopclntab` has the most
columnar redundancy to expose.

See `internal/bench/bench_test.go` to reproduce.

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
go install github.com/Tener/go-binary-compression/cmd/gobc@latest

gobc encode    helper helper.xform
gobc roundtrip helper                      # verifies encode→decode is byte-identical
gzip -9 < helper.xform > helper.xform.gz
# …later:
gzip -d < helper.xform.gz | gobc decode /dev/stdin restored
cmp helper restored                        # confirms lossless round-trip
```

## Constraints

- **x86-64 Linux ELF** only.
- **Go 1.18+ pclntab** (magic `0xFFFFFFF1`).
- **Monotonic ftab funcoffs** — the encoder errors otherwise.
- BCJ is the "always-convert" variant; the range-checked LZMA-SDK variant
  would gain ~30 KB more at the cost of reversibility edge cases.

## Package layout

    bcj/              x86 BCJ encode/decode
    pcln/             .gopclntab encode/decode (with Meta side-info)
    pipeline/         ELF-aware wrapper
    cmd/gobc/         CLI
    internal/bench/   compression-matrix tests
    testdata/         reference 9.3 MB Go binary

## Tests

```sh
go test ./...                       # round-trip tests
go test -v ./internal/bench         # compression matrix (needs gzip/xz/zstd/zpaq)
go test -bench=. ./bcj ./pipeline   # microbenchmarks
```

## Non-goals

- Not a compressor — preprocessing only; you still run a downstream
  compressor.
- Not a runtime loader — the transformed blob isn't a runnable ELF; call
  `Decode` first.
- Not a replacement for xz's `--x86` filter — use that directly if xz is
  your only compressor. The value here is BCJ + pcln in front of *any*
  compressor (including pure-Go gzip/zstd).
