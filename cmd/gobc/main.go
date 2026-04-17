// Command gobc encodes and decodes Go ELF binaries using the bcj+pcln
// transform pipeline.
//
//	gobc encode <in> <out>       transform-encode
//	gobc decode <in> <out>       reverse the transform
//	gobc roundtrip <in>          verify encode→decode recovers the input
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/Tener/go-binary-compression/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage(2)
	}
	switch os.Args[1] {
	case "encode":
		if len(os.Args) != 4 {
			usage(2)
		}
		raw := mustRead(os.Args[2])
		blob, err := pipeline.Encode(raw)
		check(err)
		check(os.WriteFile(os.Args[3], blob, 0644))
		fmt.Fprintf(os.Stderr, "encoded %d → %d bytes (Δ=%+d)\n", len(raw), len(blob), len(blob)-len(raw))
	case "decode":
		if len(os.Args) != 4 {
			usage(2)
		}
		blob := mustRead(os.Args[2])
		raw, err := pipeline.Decode(blob)
		check(err)
		check(os.WriteFile(os.Args[3], raw, 0644))
		fmt.Fprintf(os.Stderr, "decoded %d → %d bytes\n", len(blob), len(raw))
	case "roundtrip":
		if len(os.Args) != 3 {
			usage(2)
		}
		raw := mustRead(os.Args[2])
		blob, err := pipeline.Encode(raw)
		check(err)
		got, err := pipeline.Decode(blob)
		check(err)
		rs := sha256.Sum256(raw)
		gs := sha256.Sum256(got)
		fmt.Fprintf(os.Stderr, "orig sha256:     %s\n", hex.EncodeToString(rs[:]))
		fmt.Fprintf(os.Stderr, "recovered sha256: %s\n", hex.EncodeToString(gs[:]))
		if !bytes.Equal(raw, got) {
			fmt.Fprintln(os.Stderr, "ROUND-TRIP FAILED")
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "ROUND-TRIP OK")
	case "-h", "--help", "help":
		usage(0)
	default:
		usage(2)
	}
}

func usage(code int) {
	fmt.Fprintln(os.Stderr, "usage: gobc encode|decode <in> <out>")
	fmt.Fprintln(os.Stderr, "       gobc roundtrip <in>")
	os.Exit(code)
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	check(err)
	return b
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
