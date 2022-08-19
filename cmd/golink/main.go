// The golink server runs http://go/, a private shortlink service for tailnets.
package main

import (
	_ "embed"
	"log"

	"github.com/tailscale/golink"
)

//go:embed link-snapshot.json
var lastSnapshot []byte

func main() {
	golink.LastSnapshot = lastSnapshot
	if err := golink.Run(); err != nil {
		log.Fatal(err)
	}
}
