package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type platform struct{ os, arch, suffix, releaseName string }

var platforms = map[string]platform{
	"x86_64-unknown-linux-gnu": {"linux", "amd64", "", "linux-x86_64"},
	"x86_64-pc-windows-msvc":   {"windows", "amd64", ".exe", "windows-x86_64"},
	"aarch64-apple-darwin":     {"darwin", "arm64", "", "macos-aarch64"},
}

func (p platform) binaries() []string {
	return []string{"amux" + p.suffix, "amux-desktop" + p.suffix, "agent-tools" + p.suffix, "memory" + p.suffix}
}

func (p platform) archiveExtension() string {
	if p.os == "linux" {
		return ".tar.gz"
	}
	return ".zip"
}

func (p platform) archiveName(id string) string {
	return "amux-" + p.releaseName + "-" + id + p.archiveExtension()
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal().Err(err).Msg("amux-release")
	}
}

func run(args []string) error {
	command := &cli.Command{
		Name:  "amux-release",
		Usage: "Prepare and verify Amux releases",
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 0 {
				return fmt.Errorf("unknown release operation %q", cmd.Args().First())
			}
			return cli.ShowRootCommandHelp(cmd)
		},
		Commands: []*cli.Command{
			releaseCommand("tools", "Fetch bundled tools", nil, func(cmd *cli.Command, p platform) error {
				return fetchTools(cmd.String("target"), p, cmd.String("output"))
			}),
			releaseCommand("package", "Package a release", []cli.Flag{
				&cli.StringFlag{Name: "bundle", Usage: "Staged executables, license and model catalog"},
				&cli.StringFlag{Name: "version", Usage: "Release version (vMAJOR.MINOR.PATCH)"},
				&cli.StringFlag{Name: "revision", Usage: "Source commit"},
				&cli.BoolFlag{Name: "candidate-only", Usage: "Exclude from stable updates"},
			}, func(cmd *cli.Command, p platform) error {
				version, revision := cmd.String("version"), cmd.String("revision")
				if !tagPattern.MatchString(version) || !revisionPattern.MatchString(revision) {
					return fmt.Errorf("invalid release version or revision")
				}
				return seal(cmd.String("bundle"), cmd.String("target"), p, version, revision, cmd.Bool("candidate-only"), cmd.String("output"))
			}),
			releaseCommand("smoke", "Verify a packaged release", []cli.Flag{
				&cli.StringFlag{Name: "bundle", Usage: "Packaged release"},
				&cli.StringFlag{Name: "revision", Usage: "Source commit"},
			}, func(cmd *cli.Command, p platform) error {
				return smoke(cmd.String("target"), p, cmd.String("output"), cmd.String("bundle"), cmd.String("revision"))
			}),
			{
				Name:  "check-tag",
				Usage: "Validate a release tag",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if cmd.Args().Len() != 1 || !tagPattern.MatchString(cmd.Args().First()) {
						return fmt.Errorf("version must use vMAJOR.MINOR.PATCH")
					}
					return nil
				},
			},
		},
	}
	return command.Run(context.Background(), append([]string{command.Name}, args...))
}

func releaseCommand(name, usage string, flags []cli.Flag, action func(*cli.Command, platform) error) *cli.Command {
	return &cli.Command{
		Name:  name,
		Usage: usage,
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "target", Usage: "Release target"},
			&cli.StringFlag{Name: "output", Usage: "Output path"},
		}, flags...),
		Action: func(_ context.Context, cmd *cli.Command) error {
			p, ok := platforms[cmd.String("target")]
			if !ok || cmd.String("output") == "" || cmd.Args().Len() != 0 {
				return fmt.Errorf("valid --target and --output required")
			}
			return action(cmd, p)
		},
	}
}

func writeFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for name, data := range files {
		mode := os.FileMode(0644)
		switch strings.TrimSuffix(filepath.Base(name), ".exe") {
		case "amux", "amux-desktop", "agent-tools", "memory":
			mode = 0755
		}
		filename := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filename, data, mode); err != nil {
			return err
		}
	}
	return nil
}
