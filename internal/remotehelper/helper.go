package remotehelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/andrew/swh-git/internal/swh"
)

const maxCommandBytes = 8192

func Run(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: git-remote-swh <remote> [<address>]")
	}
	address := args[len(args)-1]
	for {
		command, err := readCommand(os.Stdin)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch command {
		case "capabilities":
			if _, err := fmt.Fprint(os.Stdout, "connect\n\n"); err != nil {
				return err
			}
		case "connect git-upload-pack":
			return connect(ctx, address)
		case "connect git-receive-pack":
			return errors.New("remote is read-only")
		case "":
			return nil
		default:
			return fmt.Errorf("unsupported helper command %q", command)
		}
	}
}

func readCommand(input io.Reader) (string, error) {
	var command strings.Builder
	var single [1]byte
	// Avoid consuming Git packet bytes before handing stdin to upload-pack.
	for command.Len() < maxCommandBytes {
		if _, err := io.ReadFull(input, single[:]); err != nil {
			return "", err
		}
		if single[0] == '\n' {
			return command.String(), nil
		}
		command.WriteByte(single[0])
	}
	return "", errors.New("helper command too long")
}

func connect(ctx context.Context, address string) error {
	cfg, err := swh.ConfigFromEnv()
	if err != nil {
		return err
	}
	store, err := swh.New(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	prepareCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	id, err := store.Resolve(prepareCtx, address)
	if err != nil {
		return err
	}
	repo, err := store.Ensure(prepareCtx, id)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, store.GitPath(), "upload-pack", "--strict", repo)
	cmd.Env = swh.GitEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if _, err := fmt.Fprint(os.Stdout, "\n"); err != nil {
		return err
	}
	return cmd.Run()
}
