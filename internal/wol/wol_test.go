package wol

import (
	"net"
	"testing"
)

func TestPacket(t *testing.T) {
	mac, err := net.ParseMAC("02:00:00:00:01:fa")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Packet(mac)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 102 {
		t.Fatalf("magic packet is %d bytes, want 102", len(p))
	}
	for i := 0; i < 6; i++ {
		if p[i] != 0xFF {
			t.Fatalf("byte %d is %#x, want 0xFF", i, p[i])
		}
	}
	for rep := 0; rep < 16; rep++ {
		off := 6 + rep*6
		if got := net.HardwareAddr(p[off : off+6]).String(); got != mac.String() {
			t.Fatalf("repetition %d is %s, want %s", rep, got, mac)
		}
	}
}

func TestPacketRejectsShortMAC(t *testing.T) {
	if _, err := Packet(net.HardwareAddr{1, 2, 3}); err == nil {
		t.Fatal("expected an error for a 3-byte MAC")
	}
}

func TestDirectedBroadcast(t *testing.T) {
	_, n, _ := net.ParseCIDR("192.0.2.79/24")
	if got := directedBroadcast(n).String(); got != "192.0.2.255" {
		t.Fatalf("got %s, want 192.0.2.255", got)
	}
}
