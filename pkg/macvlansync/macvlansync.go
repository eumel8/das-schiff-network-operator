// Package macvlansync watches for stale macvlan FDB entries on vlan.* host
// interfaces and immediately removes the corresponding bridge-learned entries
// from the l2.* bridge. This prevents FRR/Zebra from advertising stale EVPN
// Type-2 (MAC/IP) routes after a pod with a macvlan secondary interface is
// deleted and recreated (e.g. during StatefulSet rolling updates).
//
// Background: When a macvlan interface inside a pod network namespace is
// destroyed (pod deletion), the kernel removes the "self permanent" FDB entry
// on the host-side vlan.XXXX interface. However, the bridge-learned entry on
// the bridge port l2v.XXXX (master l2.XXXX) persists until the bridge ageing
// timer expires (default 300s). During this window FRR continues to advertise
// the old (IP, oldMAC) binding via EVPN, causing DAD failures on the
// replacement pod which gets the same IP but a new random MAC.
package macvlansync

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Syncer watches for FDB deletions on vlan.* interfaces and cleans up
// corresponding stale entries from l2.* bridges.
type Syncer struct {
	// vlanPrefix is the naming prefix for host-side macvlan master interfaces.
	vlanPrefix string

	// bridgePortPrefix is the naming prefix for the bridge-side veth
	// connecting vlan.XXXX to l2.XXXX.
	bridgePortPrefix string

	// tracked maps vlan interface indices to their bridge port info.
	tracked   map[int]*trackedInterface
	trackedMu sync.RWMutex

	// neighSubscribeFn allows injection for testing.
	neighSubscribeFn func(ch chan<- netlink.NeighUpdate, done <-chan struct{}, options netlink.NeighSubscribeOptions) error

	done chan struct{}
}

type trackedInterface struct {
	vlanName       string
	bridgePortName string
	bridgePortIdx  int
	bridgeName     string
	bridgeIdx      int
}

// New creates a Syncer with default prefixes matching the network-operator
// naming convention: vlan.XXXX host interfaces with l2v.XXXX bridge ports
// slaved to l2.XXXX bridges.
func New() *Syncer {
	return &Syncer{
		vlanPrefix:       "vlan.",
		bridgePortPrefix: "l2v.",
		tracked:          make(map[int]*trackedInterface),
		neighSubscribeFn: netlink.NeighSubscribeWithOptions,
		done:             make(chan struct{}),
	}
}

// Start discovers all vlan.* interfaces, builds the vlan→bridge mapping,
// takes a snapshot of current FDB state, and subscribes to netlink FDB
// events for immediate stale-MAC cleanup.
func (s *Syncer) Start() error {
	if err := s.discoverInterfaces(); err != nil {
		return fmt.Errorf("failed to discover interfaces: %w", err)
	}

	if len(s.tracked) == 0 {
		log.Println("macvlansync: no vlan.* interfaces found, nothing to track")
		return nil
	}

	for _, t := range s.tracked {
		log.Printf("macvlansync: tracking %s (idx=%d) → bridge port %s (idx=%d) → bridge %s (idx=%d)",
			t.vlanName, 0, t.bridgePortName, t.bridgePortIdx, t.bridgeName, t.bridgeIdx)
	}

	go s.watchFDBEvents()
	return nil
}

// Stop terminates the FDB event watcher.
func (s *Syncer) Stop() {
	close(s.done)
}

// discoverInterfaces finds all vlan.XXXX interfaces and maps them to their
// corresponding l2v.XXXX bridge ports and l2.XXXX bridges.
func (s *Syncer) discoverInterfaces() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links: %w", err)
	}

	// Index links by name for quick lookup.
	byName := make(map[string]netlink.Link, len(links))
	for _, l := range links {
		byName[l.Attrs().Name] = l
	}

	s.trackedMu.Lock()
	defer s.trackedMu.Unlock()

	for _, link := range links {
		name := link.Attrs().Name
		if !strings.HasPrefix(name, s.vlanPrefix) {
			continue
		}

		// Extract the VLAN ID suffix: vlan.1007 → 1007
		suffix := strings.TrimPrefix(name, s.vlanPrefix)

		// Find the corresponding bridge port l2v.XXXX
		bpName := s.bridgePortPrefix + suffix
		bp, ok := byName[bpName]
		if !ok {
			log.Printf("macvlansync: no bridge port %s found for %s, skipping", bpName, name)
			continue
		}

		// The bridge port's master is the l2.XXXX bridge.
		masterIdx := bp.Attrs().MasterIndex
		if masterIdx == 0 {
			log.Printf("macvlansync: bridge port %s has no master, skipping", bpName)
			continue
		}

		master, err := netlink.LinkByIndex(masterIdx)
		if err != nil {
			log.Printf("macvlansync: failed to get master for %s: %v, skipping", bpName, err)
			continue
		}

		s.tracked[link.Attrs().Index] = &trackedInterface{
			vlanName:       name,
			bridgePortName: bpName,
			bridgePortIdx:  bp.Attrs().Index,
			bridgeName:     master.Attrs().Name,
			bridgeIdx:      masterIdx,
		}
	}

	return nil
}

// watchFDBEvents subscribes to netlink neighbor (FDB) events and processes
// deletions of "self permanent" entries on tracked vlan.* interfaces.
func (s *Syncer) watchFDBEvents() {
	for {
		updates := make(chan netlink.NeighUpdate)
		watchDone := make(chan struct{})

		err := s.neighSubscribeFn(updates, watchDone, netlink.NeighSubscribeOptions{
			Namespace:    nil,
			ErrorCallback: func(err error) {
				log.Printf("macvlansync: netlink error: %v", err)
			},
		})
		if err != nil {
			log.Printf("macvlansync: failed to subscribe to FDB events: %v", err)
			return
		}
		log.Println("macvlansync: subscribed to netlink neighbor events")

		for {
			select {
			case update, ok := <-updates:
				if !ok {
					log.Println("macvlansync: update channel closed, resubscribing")
					goto resubscribe
				}
				s.processEvent(&update)
			case <-s.done:
				close(watchDone)
				return
			}
		}

	resubscribe:
		close(watchDone)
	}
}

// processEvent handles a single FDB neighbor event. When a unicast MAC with
// NTF_SELF flag is deleted from a tracked vlan.* interface, we immediately
// delete the corresponding bridge-learned entry from the l2v.* bridge port.
func (s *Syncer) processEvent(update *netlink.NeighUpdate) {
	// We only care about FDB deletions (RTM_DELNEIGH) with NTF_SELF flag
	// on tracked vlan.* interfaces.
	if update.Type != unix.RTM_DELNEIGH {
		return
	}

	// FDB entries are delivered with State == NUD_PERMANENT and NTF_SELF flag.
	// The Family field may be AF_BRIDGE or AF_UNSPEC depending on kernel version.
	// We identify FDB entries by checking NTF_SELF + NUD_PERMANENT state.
	if update.Flags&netlink.NTF_SELF == 0 {
		return
	}

	if update.State != netlink.NUD_PERMANENT {
		return
	}

	// Skip multicast/broadcast MACs.
	if !isUnicast(update.HardwareAddr) {
		return
	}

	s.trackedMu.RLock()
	info, ok := s.tracked[update.LinkIndex]
	s.trackedMu.RUnlock()
	if !ok {
		return
	}

	mac := update.HardwareAddr
	log.Printf("macvlansync: detected FDB deletion on %s: MAC %s — cleaning up bridge %s via port %s",
		info.vlanName, mac, info.bridgeName, info.bridgePortName)

	// Delete the stale bridge-learned FDB entry on the bridge port.
	if err := netlink.NeighDel(&netlink.Neigh{
		LinkIndex:    info.bridgePortIdx,
		Family:       unix.AF_BRIDGE,
		HardwareAddr: mac,
		MasterIndex:  info.bridgeIdx,
	}); err != nil {
		log.Printf("macvlansync: failed to delete FDB entry for %s on %s: %v (may already be gone)",
			mac, info.bridgePortName, err)
	} else {
		log.Printf("macvlansync: successfully removed stale FDB entry for %s from %s",
			mac, info.bridgePortName)
	}
}

// isUnicast returns true if the MAC address is a unicast address
// (least significant bit of first octet is 0).
func isUnicast(mac net.HardwareAddr) bool {
	return len(mac) >= 1 && mac[0]&0x01 == 0
}
