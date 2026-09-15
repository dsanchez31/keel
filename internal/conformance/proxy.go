package conformance

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Faults is what the suite does to the transport between an adapter and its
// vehicle, without the vehicle's cooperation: the way C6 withholds telemetry
// and C7 forces a disconnect against any vehicle, simulated or real.
type Faults interface {
	// Withhold stops (true) or resumes (false) the vehicle's traffic toward
	// the adapter, leaving the link itself up.
	Withhold(on bool)
	// Cut drops the link until Restore: open connections are closed and new
	// ones refused, datagrams are dropped both ways.
	Cut()
	Restore()
	// Close stops the proxy and every goroutine it started.
	Close() error
}

// dialTimeout bounds the proxy's connection to the vehicle.
const dialTimeout = 5 * time.Second

// TCPProxy relays TCP connections to an upstream vehicle: the native
// protocol's WebSocket runs through it unchanged. Withholding pauses the
// vehicle's stream rather than dropping bytes, which would corrupt the
// framing: what the vehicle sent meanwhile waits in the proxy and in the
// kernel's buffers, and the adapter sees silence.
type TCPProxy struct {
	ln       net.Listener
	upstream string

	mu       sync.Mutex
	withhold bool
	// resume is closed when withholding ends.
	resume chan struct{}
	cut    bool
	closed bool
	pairs  map[*pair]struct{}

	wg sync.WaitGroup
}

var _ Faults = (*TCPProxy)(nil)

// pair is one relayed connection.
type pair struct {
	down, up net.Conn
	once     sync.Once
	done     chan struct{}
}

func (p *pair) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.down.Close()
		_ = p.up.Close()
	})
}

// NewTCPProxy listens on a loopback port and relays each connection to
// upstream, a host:port.
func NewTCPProxy(upstream string) (*TCPProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("conformance: proxy: %w", err)
	}
	p := &TCPProxy{ln: ln, upstream: upstream, resume: make(chan struct{}), pairs: map[*pair]struct{}{}}
	close(p.resume)
	p.wg.Go(p.accept)
	return p, nil
}

// Addr is where the adapter connects.
func (p *TCPProxy) Addr() string { return p.ln.Addr().String() }

func (p *TCPProxy) accept() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.cut || p.closed
		p.mu.Unlock()
		if refuse {
			_ = down.Close()
			continue
		}
		up, err := net.DialTimeout("tcp", p.upstream, dialTimeout)
		if err != nil {
			_ = down.Close()
			continue
		}
		pr := &pair{down: down, up: up, done: make(chan struct{})}
		p.mu.Lock()
		if p.cut || p.closed {
			p.mu.Unlock()
			pr.close()
			continue
		}
		p.pairs[pr] = struct{}{}
		p.mu.Unlock()
		p.wg.Go(func() { p.toVehicle(pr) })
		p.wg.Go(func() { p.toAdapter(pr) })
	}
}

// toVehicle relays the adapter's bytes as they come.
func (p *TCPProxy) toVehicle(pr *pair) {
	_, _ = io.Copy(pr.up, pr.down)
	p.drop(pr)
}

// toAdapter relays the vehicle's bytes, holding each chunk while the stream
// is withheld.
func (p *TCPProxy) toAdapter(pr *pair) {
	defer p.drop(pr)
	buf := make([]byte, 32<<10)
	for {
		n, err := pr.up.Read(buf)
		if n > 0 {
			p.mu.Lock()
			resume := p.resume
			p.mu.Unlock()
			select {
			case <-resume:
			case <-pr.done:
				return
			}
			if _, werr := pr.down.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *TCPProxy) drop(pr *pair) {
	pr.close()
	p.mu.Lock()
	delete(p.pairs, pr)
	p.mu.Unlock()
}

// Withhold pauses or resumes the vehicle's stream.
func (p *TCPProxy) Withhold(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case on && !p.withhold:
		p.resume = make(chan struct{})
	case !on && p.withhold:
		close(p.resume)
	}
	p.withhold = on
}

// Cut closes every relayed connection and refuses new ones until Restore.
func (p *TCPProxy) Cut() {
	p.mu.Lock()
	p.cut = true
	pairs := p.snapshot()
	p.mu.Unlock()
	for _, pr := range pairs {
		pr.close()
	}
}

// Restore accepts connections again.
func (p *TCPProxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
}

// Close stops listening, closes every connection and waits for the relays.
// Calling it again returns nil.
func (p *TCPProxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	pairs := p.snapshot()
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, pr := range pairs {
		pr.close()
	}
	p.wg.Wait()
	return nil
}

func (p *TCPProxy) snapshot() []*pair {
	out := make([]*pair, 0, len(p.pairs))
	for pr := range p.pairs {
		out = append(out, pr)
	}
	return out
}

// UDPProxy relays datagrams between a vehicle that pushes to a fixed address,
// as ArduPilot SITL's udpclient does, and a ground station reaching the proxy
// from its own socket. It listens on the vehicle's address in the ground
// station's place, and on a loopback port the adapter's link connects to.
// Each side's peer is the last address it heard from.
type UDPProxy struct {
	vehicle *net.UDPConn
	ground  *net.UDPConn

	mu          sync.Mutex
	vehiclePeer *net.UDPAddr
	groundPeer  *net.UDPAddr
	withhold    bool
	cut         bool
	closed      bool

	wg sync.WaitGroup
}

var _ Faults = (*UDPProxy)(nil)

// maxDatagram is the largest MAVLink v2 frame, signed, with room to spare.
const maxDatagram = 2048

// NewUDPProxy listens on vehicleAddr, where the vehicle pushes, and on a
// loopback port for the ground station.
func NewUDPProxy(vehicleAddr string) (*UDPProxy, error) {
	va, err := net.ResolveUDPAddr("udp", vehicleAddr)
	if err != nil {
		return nil, fmt.Errorf("conformance: proxy: %w", err)
	}
	vehicle, err := net.ListenUDP("udp", va)
	if err != nil {
		return nil, fmt.Errorf("conformance: proxy: listening where the vehicle pushes: %w", err)
	}
	ground, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = vehicle.Close()
		return nil, fmt.Errorf("conformance: proxy: %w", err)
	}
	p := &UDPProxy{vehicle: vehicle, ground: ground}
	p.wg.Go(func() { p.relay(p.vehicle, true) })
	p.wg.Go(func() { p.relay(p.ground, false) })
	return p, nil
}

// VehicleAddr is where the vehicle pushes its datagrams.
func (p *UDPProxy) VehicleAddr() string { return p.vehicle.LocalAddr().String() }

// GroundAddr is where the adapter's link connects.
func (p *UDPProxy) GroundAddr() string { return p.ground.LocalAddr().String() }

// relay forwards what one socket receives to the other side's peer, dropping
// what the faults say to drop.
func (p *UDPProxy) relay(from *net.UDPConn, fromVehicle bool) {
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := from.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		p.mu.Lock()
		var to *net.UDPConn
		var peer *net.UDPAddr
		drop := p.cut
		if fromVehicle {
			p.vehiclePeer = addr
			drop = drop || p.withhold
			to, peer = p.ground, p.groundPeer
		} else {
			p.groundPeer = addr
			to, peer = p.vehicle, p.vehiclePeer
		}
		p.mu.Unlock()
		if drop || peer == nil {
			continue
		}
		_, _ = to.WriteToUDP(buf[:n], peer)
	}
}

// Withhold drops the vehicle's datagrams, or stops dropping them.
func (p *UDPProxy) Withhold(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.withhold = on
}

// Cut drops every datagram both ways until Restore: over a connectionless
// transport, a disconnect is an outage.
func (p *UDPProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
}

// Restore relays again.
func (p *UDPProxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
}

// Close closes both sockets and waits for the relays. Calling it again
// returns nil.
func (p *UDPProxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.vehicle.Close()
	_ = p.ground.Close()
	p.wg.Wait()
	return nil
}
