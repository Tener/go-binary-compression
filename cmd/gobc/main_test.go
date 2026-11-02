package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tener/go-binary-compression/pipeline"
)

func TestDecodeFileSpooling(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/helper_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := pipeline.Encode(raw)
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	files := t.TempDir()
	input := filepath.Join(files, "encoded")
	output := filepath.Join(files, "decoded")
	for _, samePath := range []bool{false, true} {
		if samePath {
			output = input
		}
		if err := os.WriteFile(input, blob, 0644); err != nil {
			t.Fatal(err)
		}
		if err := decodeFile(input, output); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatal("decoded bytes differ")
		}
	}
	if err := os.WriteFile(input, blob, 0644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	output = filepath.Join(files, "stdout")
	out, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, out
	decodeErr := decodeFile("-", "-")
	os.Stdin, os.Stdout = oldIn, oldOut
	closeErr := out.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("stdout bytes differ")
	}
	if err := os.WriteFile(input, []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := decodeFile(input, output); err == nil {
		t.Fatal("expected invalid input error")
	}
	if err := decodeFile(filepath.Join(files, "missing"), output); err == nil {
		t.Fatal("expected missing input error")
	}
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("spool files remain after decoding")
	}
}
