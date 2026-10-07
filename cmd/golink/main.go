// The golink server runs http://go/, a private shortlink service for tailnets.
package main

import (
	"context"
	_ "embed"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tailscale/golink"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := golink.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
