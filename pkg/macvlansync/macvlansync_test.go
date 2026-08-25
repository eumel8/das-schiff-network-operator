package macvlansync

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestIsUnicast(t *testing.T) {
	tests := []struct {
		name string
		mac  net.HardwareAddr
		want bool
	}{
		{"unicast", net.HardwareAddr{0x02, 0x03, 0x04, 0x05, 0x06, 0x07}, true},
		{"multicast", net.HardwareAddr{0x33, 0x33, 0x00, 0x00, 0x00, 0x01}, false},
		{"broadcast", net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, false},
		{"empty", net.HardwareAddr{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnicast(tt.mac); got != tt.want {
				t.Errorf("isUnicast(%v) = %v, want %v", tt.mac, got, tt.want)
			}
		})
	}
}

func TestProcessEvent_IgnoresNonDelete(t *testing.T) {
	s := &Syncer{
		tracked: map[int]*trackedInterface{
			10: {vlanName: "vlan.1007", bridgePortIdx: 20, bridgeIdx: 30},
		},
	}

	// RTM_NEWNEIGH should be ignored.
	s.processEvent(&netlink.NeighUpdate{
		Type: unix.RTM_NEWNEIGH,
		Neigh: netlink.Neigh{
			LinkIndex:    10,
			Family:       unix.AF_BRIDGE,
			Flags:        netlink.NTF_SELF,
			HardwareAddr: net.HardwareAddr{0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		},
	})
	// No panic, no action — success.
}

func TestProcessEvent_IgnoresUntrackedInterface(t *testing.T) {
	s := &Syncer{
		tracked: map[int]*trackedInterface{},
	}

	s.processEvent(&netlink.NeighUpdate{
		Type: unix.RTM_DELNEIGH,
		Neigh: netlink.Neigh{
			LinkIndex:    999,
			Family:       unix.AF_BRIDGE,
			Flags:        netlink.NTF_SELF,
			HardwareAddr: net.HardwareAddr{0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		},
	})
	// No panic, no action — success.
}

func TestProcessEvent_IgnoresMulticast(t *testing.T) {
	s := &Syncer{
		tracked: map[int]*trackedInterface{
			10: {vlanName: "vlan.1007", bridgePortIdx: 20, bridgeIdx: 30},
		},
	}

	s.processEvent(&netlink.NeighUpdate{
		Type: unix.RTM_DELNEIGH,
		Neigh: netlink.Neigh{
			LinkIndex:    10,
			Family:       unix.AF_BRIDGE,
			Flags:        netlink.NTF_SELF,
			HardwareAddr: net.HardwareAddr{0x33, 0x33, 0x00, 0x00, 0x00, 0x01},
		},
	})
	// No panic, no action — success.
}

func TestProcessEvent_IgnoresNonBridgeFamily(t *testing.T) {
	s := &Syncer{
		tracked: map[int]*trackedInterface{
			10: {vlanName: "vlan.1007", bridgePortIdx: 20, bridgeIdx: 30},
		},
	}

	s.processEvent(&netlink.NeighUpdate{
		Type: unix.RTM_DELNEIGH,
		Neigh: netlink.Neigh{
			LinkIndex:    10,
			Family:       unix.AF_INET6,
			Flags:        netlink.NTF_SELF,
			HardwareAddr: net.HardwareAddr{0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		},
	})
	// No panic, no action — success.
}
