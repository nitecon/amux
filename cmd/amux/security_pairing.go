package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/nitecon/amux/storage"
)

func runClientEnroll(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("amux client enroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	state := flags.String("state-dir", "", "directory for enrolled gateway credentials")
	serverAddress := flags.String("server", "", "gateway HTTPS origin")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *state == "" || *serverAddress == "" {
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
	flags := flag.NewFlagSet("amux client renew", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("enrolled-profile", "", "path to the enrolled client-profile.json manifest")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *profile == "" {
		return fmt.Errorf("usage: amux client renew --enrolled-profile PATH")
	}
	return storage.RenewPeerClientNow(ctx, *profile)
}
