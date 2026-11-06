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

	// Links in tsnet's workload identity federation support, so golink can
	// join a tailnet with TS_CLIENT_ID plus TS_ID_TOKEN or TS_AUDIENCE
	// instead of an auth key. Build with ts_omit_identityfederation to leave
	// it (and the cloud provider SDKs it pulls in) out.
	_ "tailscale.com/feature/condregister/identityfederation"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := golink.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
