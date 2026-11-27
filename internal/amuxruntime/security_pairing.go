package amuxruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nitecon/amux/storage"
	"github.com/urfave/cli/v3"
)

func runClientEnroll(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := commandFlags("amux client enroll", stderr)
	state := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "state-dir", Value: "", Usage: "directory for enrolled gateway credentials", Destination: state})
	serverAddress := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway HTTPS origin", Destination: serverAddress})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 || *state == "" || *serverAddress == "" {
		return fmt.Errorf("usage: amux client enroll --state-dir DIR --server HTTPS_URL (reads the peering key from stdin)")
	}
	input := bufio.NewScanner(stdin)
	input.Buffer(make([]byte, 512), 4096)
	if !input.Scan() {
		return fmt.Errorf("could not read a peering key from stdin")
	}
	enrolled, err := storage.PairRemote(ctx, *state, *serverAddress, input.Text())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, enrolled.ProfilePath)
	return err
}

func runClientRenew(ctx context.Context, args []string, stderr io.Writer) error {
	flags := commandFlags("amux client renew", stderr)
	profile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "path to the enrolled client-profile.json manifest", Destination: profile})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 || *profile == "" {
		return fmt.Errorf("usage: amux client renew --enrolled-profile PATH")
	}
	return storage.RenewPeerClientNow(ctx, *profile)
}
