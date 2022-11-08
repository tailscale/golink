// The golink server runs http://go/, a private shortlink service for tailnets.
package main

import (
	_ "embed"
	"log"

	"github.com/tailscale/golink"
)

func main() {
	if err := golink.Run(); err != nil {
		log.Fatal(err)
	}
}
