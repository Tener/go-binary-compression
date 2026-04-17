// Package bench measures compression ratios for the raw binary vs. the
// transformed blob, across the four external compressors in common use
// (gzip, xz, zstd, zpaq). Each is skipped if the binary is not installed.
//
//	go test -v ./internal/bench -run TestCompressionMatrix
//
// Outputs a table of raw size, transformed size, and compressed sizes with
// each compressor at a reasonable "high quality" setting.
package bench

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tener/go-binary-compression/pipeline"
)

const refBinary = "../../testdata/helper_linux_amd64"

type compressor struct {
	name string
	cmd  []string // stdin → stdout; output is compressed bytes
}

// Each compressor is invoked via external command. We skip if not installed.
var compressors = []compressor{
	{"gzip-9", []string{"gzip", "-9", "-c"}},
	{"xz-9e", []string{"xz", "-9e", "-c"}},
	{"zstd-22", []string{"zstd", "-q", "-22", "--ultra", "--long=30", "-c"}},
	// zpaq is file-based, handled specially below.
}

func TestCompressionMatrix(t *testing.T) {
	raw, err := os.ReadFile(refBinary)
	if err != nil {
		t.Skipf("reference binary not present: %v", err)
	}
	blob, err := pipeline.Encode(raw)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	t.Logf("%-12s %-10s %10s  vs raw", "compressor", "input", "size")
	t.Logf("%s", string(make([]byte, 52)))

	for _, c := range compressors {
		if _, err := exec.LookPath(c.cmd[0]); err != nil {
			t.Logf("%-12s (not installed)", c.name)
			continue
		}
		rawSize := runCompressor(t, c.cmd, raw)
		blobSize := runCompressor(t, c.cmd, blob)
		t.Logf("%-12s raw        %10d", c.name, rawSize)
		t.Logf("%-12s xformed    %10d  (Δ %+d, %+.1f%%)", c.name, blobSize, blobSize-rawSize, 100*float64(blobSize-rawSize)/float64(rawSize))
	}

	if _, err := exec.LookPath("zpaq"); err != nil {
		t.Logf("zpaq (not installed)")
		return
	}
	rawSize := runZpaq(t, raw)
	blobSize := runZpaq(t, blob)
	t.Logf("%-12s raw        %10d", "zpaq-m5", rawSize)
	t.Logf("%-12s xformed    %10d  (Δ %+d, %+.1f%%)", "zpaq-m5", blobSize, blobSize-rawSize, 100*float64(blobSize-rawSize)/float64(rawSize))
}

func runCompressor(t *testing.T, argv []string, data []byte) int {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return out.Len()
}

func runZpaq(t *testing.T, data []byte) int {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "h")
	if err := os.WriteFile(in, data, 0644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "a.zpaq")
	cmd := exec.Command("zpaq", "a", out, in, "-m5")
	cmd.Stderr = io.Discard
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("zpaq: %v", err)
	}
	fi, _ := os.Stat(out)
	return int(fi.Size())
}

// BenchmarkPipeline measures pure transform-encode/decode speed, excluding
// any downstream compressor.
func BenchmarkPipeline(b *testing.B) {
	raw, err := os.ReadFile(refBinary)
	if err != nil {
		b.Skipf("reference binary not present: %v", err)
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		blob, err := pipeline.Encode(raw)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := pipeline.Decode(blob); err != nil {
			b.Fatal(err)
		}
	}
}
