package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateBundleRoundTripAndTamper(t *testing.T) {
	const target = "x86_64-pc-windows-msvc"
	p := platforms[target]
	root := t.TempDir()
	input := filepath.Join(root, "input")
	files := map[string][]byte{"LICENSE": []byte("license"), "models.json": []byte("{}")}
	for _, name := range p.binaries() {
		files[name] = []byte(name)
	}
	if err := writeFiles(input, files); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "candidate.tar.gz")
	revision := strings.Repeat("a", 40)
	if err := seal(input, target, p, "v1.2.3", revision, true, output); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := selectArchive(archive, false, flatName)
	if err != nil {
		t.Fatal(err)
	}
	build, err := verifyBundle(staged, target, p)
	if err != nil || !build.CandidateOnly || build.Revision != revision {
		t.Fatalf("candidate metadata %+v, error %v", build, err)
	}
	staged["amux.exe"] = []byte("replaced")
	if _, err := verifyBundle(staged, target, p); err == nil {
		t.Fatal("bundle with a replaced binary passed verification")
	}
}

func TestArchiveSelectionNeverUsesMemberDirectories(t *testing.T) {
	var archive bytes.Buffer
	if err := writeArchive(&archive, map[string][]byte{"../amux": []byte("bad"), "amux": []byte("good")}, nil); err != nil {
		t.Fatal(err)
	}
	files, err := selectArchive(archive.Bytes(), false, flatName)
	if err != nil || len(files) != 1 || string(files["amux"]) != "good" {
		t.Fatalf("selected files %v, error %v", files, err)
	}
}
