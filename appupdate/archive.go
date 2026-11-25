package appupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func bundledToolNames(triple string) []string {
	if triple == "x86_64-pc-windows-msvc" {
		return []string{"agent-tools.exe", "memory.exe"}
	}
	return []string{"agent-tools", "memory"}
}

func extractArchive(data []byte, destination, triple string) error {
	seen := make(map[string]bool)
	var total int64
	extract := func(name string, mode os.FileMode, size int64, reader io.Reader) error {
		name = strings.TrimSuffix(name, "/")
		key := strings.ToLower(name)
		if name != path.Clean(name) || !filepath.IsLocal(filepath.FromSlash(name)) ||
			strings.ContainsAny(name, "\\:") || seen[key] || len(seen) >= 128 ||
			size < 0 || size > maxReleaseBody-total {
			return fmt.Errorf("archive contains invalid member %q", name)
		}
		seen[key] = true
		total += size
		member := filepath.Join(destination, filepath.FromSlash(name))
		if triple == "aarch64-apple-darwin" {
			if name != "amux.app" && !strings.HasPrefix(name, "amux.app/") {
				return fmt.Errorf("archive member %q is outside amux.app", name)
			}
			if mode.IsDir() {
				return os.MkdirAll(member, 0o755)
			}
		} else if !flatMember(name, triple) {
			return fmt.Errorf("archive contains unexpected member %q", name)
		}
		if !mode.IsRegular() {
			return fmt.Errorf("archive member %q is not a regular file", name)
		}
		if err := os.MkdirAll(filepath.Dir(member), 0o755); err != nil {
			return err
		}
		permissions := os.FileMode(0o600)
		if mode&0o111 != 0 {
			permissions = 0o755
		}
		output, err := os.OpenFile(member, os.O_WRONLY|os.O_CREATE|os.O_EXCL, permissions)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(reader, size+1))
		if err := errors.Join(copyErr, output.Close()); err != nil {
			return err
		}
		if written != size {
			return fmt.Errorf("archive member %q size differs from its header", name)
		}
		return nil
	}

	if archiveExtension(triple) == ".zip" {
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return err
		}
		for _, member := range archive.File {
			reader, err := member.Open()
			if err != nil {
				return err
			}
			err = extract(member.Name, member.Mode(), int64(member.UncompressedSize64), reader)
			if err := errors.Join(err, reader.Close()); err != nil {
				return err
			}
		}
		return nil
	}

	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	for {
		member, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if member.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive member %q is not a regular file", member.Name)
		}
		if err := extract(member.Name, member.FileInfo().Mode(), member.Size, archive); err != nil {
			return err
		}
	}
}

func flatMember(name, triple string) bool {
	switch name {
	case "LICENSE", "ICED_TERM_LICENSE", "REVIEW.md", "notification-validation.md", "models.json", "BUILD.json", "SHA256SUMS":
		return true
	}
	switch triple {
	case "x86_64-unknown-linux-gnu":
		return name == "amux" || name == "amux-desktop" || name == "agent-tools" || name == "memory"
	case "x86_64-pc-windows-msvc":
		return name == "amux.exe" || name == "amux-desktop.exe" || name == "agent-tools.exe" || name == "memory.exe"
	}
	return false
}
