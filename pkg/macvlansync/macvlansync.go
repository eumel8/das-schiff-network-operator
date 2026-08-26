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
	"time"

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
		done:             make(chan struct{}),
	}
}

// Start discovers all vlan.* interfaces, builds the vlan→bridge mapping,
// takes a snapshot of current FDB state, and starts polling for stale MACs.
func (s *Syncer) Start() error {
	if err := s.discoverInterfaces(); err != nil {
		return fmt.Errorf("failed to discover interfaces: %w", err)
	}

	if len(s.tracked) == 0 {
		log.Println("macvlansync: no vlan.* interfaces found, nothing to track")
		return nil
	}

	for idx, t := range s.tracked {
		log.Printf("macvlansync: tracking %s (idx=%d) → bridge port %s (idx=%d) → bridge %s (idx=%d)",
			t.vlanName, idx, t.bridgePortName, t.bridgePortIdx, t.bridgeName, t.bridgeIdx)
	}

	go s.pollFDB()
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

// pollFDB periodically reads the FDB of each tracked vlan.* interface and
// removes stale bridge-learned entries from the corresponding l2v.* bridge
// port. Two checks are performed each cycle:
//
//  1. Diff check: MACs that disappeared from vlan.* since last poll are
//     immediately deleted from the bridge port.
//  2. Consistency check: Any unicast MAC on the bridge port that does NOT
//     have a matching "self permanent" entry on vlan.* is stale and gets
//     deleted. This catches pre-existing stale entries from before the
//     watcher started.
func (s *Syncer) pollFDB() {
	log.Println("macvlansync: starting FDB poll loop (interval=1s)")

	// previousMACs holds the last-seen set of self-permanent unicast MACs
	// per tracked vlan interface index.
	previousMACs := make(map[int]map[string]struct{})

	// Initialize with current state and run an initial consistency cleanup.
	s.trackedMu.RLock()
	for idx, info := range s.tracked {
		macs := s.readSelfPermanentMACs(info.vlanName, idx)
		previousMACs[idx] = macs
		s.cleanStaleBridgePortEntries(info, macs)
	}
	s.trackedMu.RUnlock()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.trackedMu.RLock()
			for idx, info := range s.tracked {
				currentMACs := s.readSelfPermanentMACs(info.vlanName, idx)

				// Diff check: find MACs that disappeared since last poll.
				prev := previousMACs[idx]
				for macStr := range prev {
					if _, exists := currentMACs[macStr]; !exists {
						s.deleteBridgePortFDB(info, macStr, "disappeared from "+info.vlanName)
					}
				}

				// Consistency check: remove bridge-port entries without
				// a matching self-permanent on the vlan interface.
				s.cleanStaleBridgePortEntries(info, currentMACs)

				previousMACs[idx] = currentMACs
			}
			s.trackedMu.RUnlock()
		case <-s.done:
			return
		}
	}
}

// cleanStaleBridgePortEntries reads the bridge-port FDB and deletes any
// unicast entry that has no corresponding self-permanent MAC on the vlan
// interface.
func (s *Syncer) cleanStaleBridgePortEntries(info *trackedInterface, validMACs map[string]struct{}) {
	neighs, err := netlink.NeighList(info.bridgePortIdx, unix.AF_BRIDGE)
	if err != nil {
		return
	}
	for i := range neighs {
		n := &neighs[i]
		// Only look at learned unicast entries (master set, not permanent).
		if n.MasterIndex == 0 || !isUnicast(n.HardwareAddr) {
			continue
		}
		// Skip permanent entries (e.g. the bridge port's own MAC).
		if n.Flags&netlink.NTF_SELF != 0 {
			continue
		}
		macStr := n.HardwareAddr.String()
		if _, valid := validMACs[macStr]; !valid {
			s.deleteBridgePortFDB(info, macStr, "stale on bridge port "+info.bridgePortName)
		}
	}
}

// deleteBridgePortFDB deletes a single MAC from a bridge port's FDB.
func (s *Syncer) deleteBridgePortFDB(info *trackedInterface, macStr string, reason string) {
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return
	}
	log.Printf("macvlansync: MAC %s %s — deleting from bridge port %s",
		macStr, reason, info.bridgePortName)

	if err := netlink.NeighDel(&netlink.Neigh{
		LinkIndex:    info.bridgePortIdx,
		Family:       unix.AF_BRIDGE,
		HardwareAddr: mac,
		MasterIndex:  info.bridgeIdx,
	}); err != nil {
		log.Printf("macvlansync: failed to delete FDB entry for %s on %s: %v (may already be gone)",
			macStr, info.bridgePortName, err)
	} else {
		log.Printf("macvlansync: successfully removed stale FDB entry for %s from %s",
			macStr, info.bridgePortName)
	}
}

// readSelfPermanentMACs reads the FDB of the given interface and returns
// all unicast MACs that have the "self permanent" flags.
func (s *Syncer) readSelfPermanentMACs(name string, linkIdx int) map[string]struct{} {
	result := make(map[string]struct{})

	neighs, err := netlink.NeighList(linkIdx, unix.AF_BRIDGE)
	if err != nil {
		log.Printf("macvlansync: failed to list FDB for %s (idx=%d): %v", name, linkIdx, err)
		return result
	}

	for i := range neighs {
		n := &neighs[i]
		// Match "self permanent" unicast entries: MasterIndex==0 means it's on
		// the interface itself (self), not on a bridge port.
		if n.MasterIndex == 0 && n.Flags&netlink.NTF_SELF != 0 && isUnicast(n.HardwareAddr) {
			result[n.HardwareAddr.String()] = struct{}{}
		}
	}

	return result
}

// isUnicast returns true if the MAC address is a unicast address
// (least significant bit of first octet is 0).
func isUnicast(mac net.HardwareAddr) bool {
	return len(mac) >= 1 && mac[0]&0x01 == 0
}
