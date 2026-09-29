package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/andrew/swh-git/internal/remotehelper"
)

func main() {
	log.SetPrefix("git-remote-swh: ")
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := remotehelper.Run(ctx, os.Args[1:])
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
