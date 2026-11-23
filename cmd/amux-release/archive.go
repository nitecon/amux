package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nitecon/amux/appupdate"
)

const sizeLimit = 512 << 20

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func readBounded(reader io.Reader) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, sizeLimit+1))
	if len(contents) > sizeLimit {
		return nil, fmt.Errorf("release data exceeds size limit")
	}
	return contents, err
}

func seal(dir, target string, p platform, tag, revision string, candidate bool, output string) error {
	names := append(p.binaries(), "LICENSE", "models.json")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != len(names) {
		return fmt.Errorf("bundle is incomplete or contains unexpected files")
	}
	files := make(map[string][]byte)
	for _, name := range names {
		filename := filepath.Join(dir, name)
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() || info.Size() > sizeLimit {
			return fmt.Errorf("invalid bundle file %s", name)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		files[name] = data
	}
	cli, gui, tools, memory := names[0], names[1], names[2], names[3]
	build := appupdate.Build{Revision: revision, Version: strings.TrimPrefix(tag, "v"), Tag: tag, Target: target, GoOS: p.os, GoArch: p.arch, GUIBinary: gui,
		BinarySHA256: digest(files[cli]), GUIBinarySHA256: digest(files[gui]), BundledSHA256: map[string]string{tools: digest(files[tools]), memory: digest(files[memory])}, ModelCatalogSHA256: digest(files["models.json"]), CandidateOnly: candidate}
	data, err := json.MarshalIndent(build, "", "  ")
	if err != nil {
		return err
	}
	files["BUILD.json"] = append(data, '\n')
	var sums strings.Builder
	for _, name := range names[:4] {
		fmt.Fprintf(&sums, "%s  %s\n", digest(files[name]), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	dest, err := os.CreateTemp(filepath.Dir(output), ".amux-bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(dest.Name())
	if err = writeArchive(dest, files, names[:4]); err != nil {
		_ = dest.Close()
		return err
	}
	if err = dest.Close(); err != nil {
		return err
	}
	return os.Rename(dest.Name(), output)
}

func writeArchive(dest io.Writer, files map[string][]byte, binaries []string) error {
	sorted := make([]string, 0, len(files))
	for name := range files {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	gz := gzip.NewWriter(dest)
	archive := tar.NewWriter(gz)
	for _, name := range sorted {
		mode := int64(0644)
		for _, binary := range binaries {
			if name == binary {
				mode = 0755
				break
			}
		}
		data := files[name]
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := archive.Write(data); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// selectArchive reads selected regular files without extracting any archive paths.
func selectArchive(data []byte, zipped bool, selectName func(string) string) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	collect := func(name string, reader io.Reader) error {
		if _, exists := files[name]; exists {
			return fmt.Errorf("duplicate archive member %s", name)
		}
		contents, err := readBounded(reader)
		if err != nil {
			return err
		}
		total += len(contents)
		if total > sizeLimit {
			return fmt.Errorf("archive expands beyond size limit")
		}
		files[name] = contents
		return nil
	}
	if zipped {
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, member := range archive.File {
			name := selectName(member.Name)
			if name == "" || !member.Mode().IsRegular() {
				continue
			}
			reader, err := member.Open()
			if err != nil {
				return nil, err
			}
			err = collect(name, reader)
			reader.Close()
			if err != nil {
				return nil, err
			}
		}
	} else {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		archive := tar.NewReader(gz)
		for {
			member, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name := selectName(member.Name)
			if name != "" && (member.Typeflag == tar.TypeReg || member.Typeflag == tar.TypeRegA) {
				if err = collect(name, archive); err != nil {
					return nil, err
				}
			}
		}
	}
	return files, nil
}

func flatName(name string) string {
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\:") {
		return ""
	}
	return name
}
