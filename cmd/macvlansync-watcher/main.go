// macvlansync-watcher is a standalone daemon that watches for stale macvlan
// FDB entries and immediately cleans them up to prevent EVPN Type-2 route
// propagation delays causing IPv6 DAD failures.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/telekom/das-schiff-network-operator/pkg/macvlansync"
)

func main() {
	log.Println("macvlansync-watcher starting")

	syncer := macvlansync.New()
	if err := syncer.Start(); err != nil {
		log.Fatalf("failed to start macvlan FDB syncer: %v", err)
	}

	log.Println("macvlansync-watcher running — waiting for signals")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("macvlansync-watcher shutting down")
	syncer.Stop()
}
