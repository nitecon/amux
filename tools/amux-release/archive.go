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
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nitecon/amux/update"
)

const sizeLimit = 512 << 20
const maxArchiveSize = 256 << 20
const maxMemberSize = 128 << 20

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
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxMemberSize {
			return fmt.Errorf("invalid bundle file %s", name)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		files[name] = data
	}
	if p.os == "darwin" {
		return sealMacApp(files, target, p, tag, revision, candidate, output)
	}
	if err := addBuildMetadata(files, target, p, tag, revision, candidate); err != nil {
		return err
	}
	return saveArchive(output, files, p.binaries(), p.archiveExtension() == ".zip")
}

func addBuildMetadata(files map[string][]byte, target string, p platform, tag, revision string, candidate bool) error {
	names := p.binaries()
	cli, gui, tools, memory := names[0], names[1], names[2], names[3]
	build := update.Build{Revision: revision, Version: strings.TrimPrefix(tag, "v"), Tag: tag, Target: target, GoOS: p.os, GoArch: p.arch, GUIBinary: gui,
		BinarySHA256: digest(files[cli]), GUIBinarySHA256: digest(files[gui]), BundledSHA256: map[string]string{tools: digest(files[tools]), memory: digest(files[memory])}, ModelCatalogSHA256: digest(files["models.json"]), CandidateOnly: candidate}
	if p.os == "darwin" {
		// Signing the app replaces the desktop binary's signature. The app
		// signature protects the desktop binary and its bundle metadata.
		build.GUIBinarySHA256 = ""
	}
	data, err := json.MarshalIndent(build, "", "  ")
	if err != nil {
		return err
	}
	files["BUILD.json"] = append(data, '\n')
	if p.os == "darwin" {
		return nil
	}
	var sums strings.Builder
	for _, name := range names {
		fmt.Fprintf(&sums, "%s  %s\n", digest(files[name]), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	return nil
}

const macApp = "amux.app"
const macExecutables = macApp + "/Contents/MacOS"
const macResources = macApp + "/Contents/Resources"

func sealMacApp(files map[string][]byte, target string, p platform, tag, revision string, candidate bool, output string) error {
	root, err := os.MkdirTemp("", "amux-release-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	binDir := filepath.Join(root, macExecutables)
	resDir := filepath.Join(root, macResources)
	binaries := p.binaries()
	executables := make(map[string][]byte, len(binaries))
	for _, name := range binaries {
		executables[name] = files[name]
	}
	if err := writeFiles(binDir, executables); err != nil {
		return err
	}
	// The app signature seals the desktop and its resources. Sign the
	// remaining executables first so BUILD.json records their final bytes.
	for _, name := range []string{binaries[0], binaries[2], binaries[3]} {
		filename := filepath.Join(binDir, name)
		if err := runTool("/usr/bin/codesign", "--force", "--sign", "-", filename); err != nil {
			return err
		}
		files[name], err = os.ReadFile(filename)
		if err != nil {
			return err
		}
	}
	if err := addBuildMetadata(files, target, p, tag, revision, candidate); err != nil {
		return err
	}
	if err := writeFiles(resDir, map[string][]byte{
		"BUILD.json": files["BUILD.json"], "models.json": files["models.json"], "LICENSE": files["LICENSE"],
	}); err != nil {
		return err
	}
	info, err := os.ReadFile("packaging/macos/Info.plist")
	if err != nil {
		return err
	}
	info = bytes.ReplaceAll(info, []byte("__VERSION__"), []byte(strings.TrimPrefix(tag, "v")))
	if err := os.WriteFile(filepath.Join(root, macApp, "Contents", "Info.plist"), info, 0644); err != nil {
		return err
	}
	if err := makeMacIcon(root, resDir); err != nil {
		return err
	}
	app := filepath.Join(root, macApp)
	if err := runTool("/usr/bin/codesign", "--force", "--sign", "-", app); err != nil {
		return err
	}
	if err := verifyMacSignature(app); err != nil {
		return err
	}
	members := make(map[string][]byte)
	err = filepath.WalkDir(app, func(filename string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected macOS bundle member %s", name)
		}
		members[filepath.ToSlash(name)], err = os.ReadFile(filename)
		return err
	})
	if err != nil {
		return err
	}
	for i, name := range binaries {
		binaries[i] = path.Join(macExecutables, name)
	}
	return saveArchive(output, members, binaries, true)
}

func makeMacIcon(root, resources string) error {
	iconset := filepath.Join(root, "amux.iconset")
	if err := os.Mkdir(iconset, 0755); err != nil {
		return err
	}
	for _, size := range []int{16, 32, 128, 256, 512} {
		for _, scale := range []int{1, 2} {
			suffix := ""
			if scale == 2 {
				suffix = "@2x"
			}
			dimension := fmt.Sprint(size * scale)
			name := fmt.Sprintf("icon_%dx%d%s.png", size, size, suffix)
			if err := runTool("/usr/bin/sips", "-z", dimension, dimension, "assets/icons/amux.png", "--out", filepath.Join(iconset, name)); err != nil {
				return err
			}
		}
	}
	return runTool("/usr/bin/iconutil", "-c", "icns", iconset, "-o", filepath.Join(resources, "amux.icns"))
}

func verifyMacSignature(app string) error {
	return runTool("/usr/bin/codesign", "--verify", "--strict", app)
}

func saveArchive(output string, files map[string][]byte, binaries []string, zipped bool) error {
	if len(files) > 64 {
		return fmt.Errorf("release archive has too many files")
	}
	expanded := 0
	for name, data := range files {
		if len(data) > maxMemberSize {
			return fmt.Errorf("release archive member %s exceeds the size limit", name)
		}
		expanded += len(data)
		if !zipped {
			// tar adds a 512-byte header and pads each payload to a block.
			expanded += 512 + (512-len(data)%512)%512
		}
	}
	if !zipped {
		expanded += 1024 // tar end-of-archive marker
	}
	if expanded > maxArchiveSize {
		return fmt.Errorf("expanded release archive exceeds the size limit")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	dest, err := os.CreateTemp(filepath.Dir(output), ".amux-bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(dest.Name())
	if zipped {
		err = writeZip(dest, files, binaries)
	} else {
		err = writeArchive(dest, files, binaries)
	}
	if err != nil {
		_ = dest.Close()
		return err
	}
	info, err := dest.Stat()
	if err != nil || info.Size() > maxArchiveSize {
		_ = dest.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf("compressed release archive exceeds the size limit")
	}
	if err = dest.Close(); err != nil {
		return err
	}
	return os.Rename(dest.Name(), output)
}

func sortedNames(files map[string][]byte) []string {
	sorted := make([]string, 0, len(files))
	for name := range files {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted
}

func memberMode(name string, binaries []string) os.FileMode {
	for _, binary := range binaries {
		if name == binary {
			return 0755
		}
	}
	return 0644
}

func writeZip(dest io.Writer, files map[string][]byte, binaries []string) error {
	archive := zip.NewWriter(dest)
	for _, name := range sortedNames(files) {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(memberMode(name, binaries))
		member, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := member.Write(files[name]); err != nil {
			return err
		}
	}
	return archive.Close()
}

func writeArchive(dest io.Writer, files map[string][]byte, binaries []string) error {
	gz := gzip.NewWriter(dest)
	archive := tar.NewWriter(gz)
	for _, name := range sortedNames(files) {
		data := files[name]
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: int64(memberMode(name, binaries)), Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
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

func runTool(path string, args ...string) error {
	if output, err := exec.Command(path, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", filepath.Base(path), err, output)
	}
	return nil
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
