package conformance_test

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dsanchez31/keel/internal/conformance"
)

// quiet is how long a test waits to conclude nothing arrives.
const quiet = 200 * time.Millisecond

// echo is a TCP server writing back every line it reads.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func line(t *testing.T, r *bufio.Reader, c net.Conn, within time.Duration) (string, error) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	return r.ReadString('\n')
}

func TestTCPProxyRelaysWithholdsAndCuts(t *testing.T) {
	p, err := conformance.NewTCPProxy(echo(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := line(t, r, c, time.Second); err != nil || got != "ping\n" {
		t.Fatalf("relayed %q, %v", got, err)
	}

	p.Withhold(true)
	if _, err := io.WriteString(c, "held\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := line(t, r, c, quiet); err == nil {
		t.Fatalf("withheld stream delivered %q", got)
	}
	p.Withhold(false)
	if got, err := line(t, r, c, time.Second); err != nil || got != "held\n" {
		t.Fatalf("after withholding, %q, %v; want the held line", got, err)
	}

	p.Cut()
	if _, err := line(t, r, c, time.Second); err == nil {
		t.Fatal("the connection survived the cut")
	}
	refused, err := net.Dial("tcp", p.Addr())
	if err == nil {
		_ = refused.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := refused.Read(make([]byte, 1)); err == nil {
			t.Fatal("a connection was relayed during the cut")
		}
		_ = refused.Close()
	}
	p.Restore()
	again, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	ra := bufio.NewReader(again)
	if _, err := io.WriteString(again, "back\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := line(t, ra, again, time.Second); err != nil || got != "back\n" {
		t.Fatalf("after the restore, %q, %v", got, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func udp(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func send(t *testing.T, from *net.UDPConn, to, msg string) {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := from.WriteToUDP([]byte(msg), a); err != nil {
		t.Fatal(err)
	}
}

func recvUDP(c *net.UDPConn, within time.Duration) (string, bool) {
	_ = c.SetReadDeadline(time.Now().Add(within))
	b := make([]byte, 64)
	n, _, err := c.ReadFromUDP(b)
	if err != nil {
		return "", false
	}
	return string(b[:n]), true
}

func TestUDPProxyRelaysWithholdsAndCuts(t *testing.T) {
	p, err := conformance.NewUDPProxy("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	vehicle, ground := udp(t), udp(t)

	// Each side's peer is whoever spoke last: the ground station first, as a
	// link announces itself with its heartbeat. Its socket and the vehicle's
	// are read by two goroutines, so a frame may reach the proxy before the
	// hello does and be dropped, peer unknown: the vehicle pushes until one
	// gets through, as a vehicle streaming telemetry does.
	send(t, ground, p.GroundAddr(), "hello")
	for deadline := time.Now().Add(time.Second); ; {
		send(t, vehicle, p.VehicleAddr(), "frame")
		if got, ok := recvUDP(ground, 20*time.Millisecond); ok {
			if got != "frame" {
				t.Fatalf("ground received %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no frame relayed to the ground")
		}
	}
	// A frame relayed late would pass for the withheld one below, and the
	// hello, heard after a frame, was relayed to the vehicle.
	for _, c := range []*net.UDPConn{ground, vehicle} {
		for {
			if _, ok := recvUDP(c, 50*time.Millisecond); !ok {
				break
			}
		}
	}
	send(t, ground, p.GroundAddr(), "command")
	if got, ok := recvUDP(vehicle, time.Second); !ok || got != "command" {
		t.Fatalf("vehicle received %q, %v", got, ok)
	}

	p.Withhold(true)
	send(t, vehicle, p.VehicleAddr(), "withheld")
	if got, ok := recvUDP(ground, quiet); ok {
		t.Fatalf("withheld datagram delivered: %q", got)
	}
	send(t, ground, p.GroundAddr(), "still commanded")
	if got, ok := recvUDP(vehicle, time.Second); !ok || got != "still commanded" {
		t.Fatalf("withholding stopped the commands too: %q, %v", got, ok)
	}
	p.Withhold(false)

	p.Cut()
	send(t, vehicle, p.VehicleAddr(), "cut up")
	send(t, ground, p.GroundAddr(), "cut down")
	if got, ok := recvUDP(ground, quiet); ok {
		t.Fatalf("datagram delivered to the ground during the cut: %q", got)
	}
	if got, ok := recvUDP(vehicle, quiet); ok {
		t.Fatalf("datagram delivered to the vehicle during the cut: %q", got)
	}
	p.Restore()
	send(t, vehicle, p.VehicleAddr(), "restored")
	if got, ok := recvUDP(ground, time.Second); !ok || got != "restored" {
		t.Fatalf("after the restore, ground received %q, %v", got, ok)
	}
}
