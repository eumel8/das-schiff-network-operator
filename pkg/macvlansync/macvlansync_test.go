package macvlansync

import (
	"net"
	"testing"
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

func TestNew(t *testing.T) {
	s := New()
	if s.vlanPrefix != "vlan." {
		t.Errorf("expected vlanPrefix 'vlan.', got %q", s.vlanPrefix)
	}
	if s.bridgePortPrefix != "l2v." {
		t.Errorf("expected bridgePortPrefix 'l2v.', got %q", s.bridgePortPrefix)
	}
	if s.tracked == nil {
		t.Error("tracked map is nil")
	}
	if s.done == nil {
		t.Error("done channel is nil")
	}
}
