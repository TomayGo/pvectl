// Package wol sends Wake-on-LAN magic packets.
//
// The nodes bond two NICs in balance-alb (mode 6) with wol=g on both slaves.
// While the machine is off, both PHYs sit waiting and the switch's choice of
// which one to deliver to is not something we can predict -- so every MAC of
// the target gets its own packet.
package wol

import (
	"context"
	"fmt"
	"net"
	"syscall"

	plog "github.com/TomayGo/pvectl/internal/log"
)

// Ports 9 (discard) and 7 (echo) are both conventional for WoL; firmware
// differs on which it listens to, and an extra UDP datagram costs nothing.
var ports = []int{9, 7}

// Packet builds the magic packet: six 0xFF bytes then the MAC sixteen times.
func Packet(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("MAC %s is not 6 bytes", mac)
	}
	buf := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		buf = append(buf, 0xFF)
	}
	for i := 0; i < 16; i++ {
		buf = append(buf, mac...)
	}
	return buf, nil
}

// Wake sends the magic packet for every MAC to every plausible broadcast
// address. targetIP is used only to work out which local subnet the node lives
// on, so the directed broadcast reaches the right VLAN.
func Wake(macs []string, targetIP string, lg *plog.Logger, dryRun bool) error {
	if len(macs) == 0 {
		return fmt.Errorf("no WoL MAC configured for this node")
	}
	dests := broadcastAddrs(targetIP)
	if dryRun {
		lg.Infof("(dry-run) would send WoL to %v via %v", macs, dests)
		return nil
	}
	conn, err := broadcastConn()
	if err != nil {
		return err
	}
	defer conn.Close()

	sent := 0
	var firstErr error
	for _, m := range macs {
		hw, err := net.ParseMAC(m)
		if err != nil {
			return fmt.Errorf("parse MAC %q: %w", m, err)
		}
		pkt, err := Packet(hw)
		if err != nil {
			return err
		}
		for _, d := range dests {
			for _, port := range ports {
				addr := &net.UDPAddr{IP: d, Port: port}
				if _, err := conn.WriteTo(pkt, addr); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					lg.Debugf("WoL %s -> %s failed: %v", m, addr, err)
					continue
				}
				sent++
			}
		}
		lg.Infof("magic packet sent to %s", m)
	}
	if sent == 0 {
		return fmt.Errorf("no WoL packet could be sent: %w", firstErr)
	}
	return nil
}

// broadcastConn opens a UDP socket with SO_BROADCAST set. Go does not set that
// option for us, and without it every send to a broadcast address fails with
// EACCES.
func broadcastConn() (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("open WoL socket: %w", err)
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("unexpected socket type %T", pc)
	}
	return uc, nil
}

// broadcastAddrs returns the limited broadcast plus the directed broadcast of
// every local IPv4 network that contains targetIP.
func broadcastAddrs(targetIP string) []net.IP {
	out := []net.IP{net.IPv4bcast}
	tip := net.ParseIP(targetIP)
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if tip != nil && !ipnet.Contains(tip) {
				continue
			}
			b := directedBroadcast(ipnet)
			if b != nil && !containsIP(out, b) {
				out = append(out, b)
			}
		}
	}
	return out
}

func directedBroadcast(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	mask := net.IP(n.Mask).To4()
	if ip == nil || mask == nil {
		return nil
	}
	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip[i] | ^mask[i]
	}
	return b
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, e := range list {
		if e.Equal(ip) {
			return true
		}
	}
	return false
}
