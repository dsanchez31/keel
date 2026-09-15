package conformance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/bluenviron/gomavlib/v4"

	"github.com/dsanchez31/keel/adapters/mavlink"
	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/vector"
)

// Subject is one vehicle as the suite tests it: an adapter it opens itself,
// over a transport it controls.
type Subject struct {
	// Name says what is under test, for the report.
	Name string
	// Faults is the proxy between the adapter and the vehicle.
	Faults Faults
	// Open returns a new adapter reaching the vehicle through Faults. Closing
	// the adapter releases everything Open started; the proxy stays.
	Open func(ctx context.Context) (vector.Vector, error)
}

// Close stops the subject's proxy.
func (s *Subject) Close() error { return s.Faults.Close() }

// NativeSubject tests the native adapter of the vehicle at vehicleURL, a
// ws:// or wss:// URL naming one vector (spec section 7.3). The adapter dials
// the URL unchanged, Host and TLS server name included, and its connections
// are routed through a TCP proxy. opts tune the adapter; their HTTPClient
// must be nil, the suite's routes through the proxy.
func NativeSubject(vehicleURL string, opts native.Options) (*Subject, error) {
	if opts.HTTPClient != nil {
		return nil, errors.New("conformance: native options carry an HTTP client, the suite routes through its own")
	}
	u, err := url.Parse(vehicleURL)
	if err != nil {
		return nil, fmt.Errorf("conformance: vehicle URL: %w", err)
	}
	port := u.Port()
	switch {
	case u.Scheme == "ws" && port == "":
		port = "80"
	case u.Scheme == "wss" && port == "":
		port = "443"
	case u.Scheme != "ws" && u.Scheme != "wss":
		return nil, fmt.Errorf("conformance: vehicle URL %q, want ws:// or wss://", vehicleURL)
	}
	proxy, err := NewTCPProxy(net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	return &Subject{
		Name:   "native " + vehicleURL,
		Faults: proxy,
		Open: func(ctx context.Context) (vector.Vector, error) {
			tr := &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "tcp", proxy.Addr())
				},
			}
			o := opts
			o.HTTPClient = &http.Client{Transport: tr}
			a, err := native.Dial(ctx, vehicleURL, o)
			if err != nil {
				tr.CloseIdleConnections()
				return nil, err
			}
			return &nativeVector{Adapter: a, transport: tr}, nil
		},
	}, nil
}

// nativeVector releases the suite's HTTP transport with the adapter.
type nativeVector struct {
	*native.Adapter
	transport *http.Transport
}

func (v *nativeVector) Close() error {
	err := v.Adapter.Close()
	v.transport.CloseIdleConnections()
	return err
}

// MAVLinkSubject tests the MAVLink adapter of the vehicle cfg describes, which
// pushes its datagrams to vehicleAddr as an ArduPilot SITL's udpclient does.
// The suite listens there in the ground station's place, so nothing else may
// hold the port, and each adapter it opens gets a link of its own through
// the proxy.
func MAVLinkSubject(vehicleAddr string, cfg mavlink.Config) (*Subject, error) {
	proxy, err := NewUDPProxy(vehicleAddr)
	if err != nil {
		return nil, err
	}
	return &Subject{
		Name:   fmt.Sprintf("MAVLink system %d via %s", cfg.SystemID, vehicleAddr),
		Faults: proxy,
		Open: func(context.Context) (vector.Vector, error) {
			link, err := mavlink.NewLink(mavlink.LinkOptions{
				Endpoints: []gomavlib.Endpoint{&gomavlib.EndpointUDPClient{Address: proxy.GroundAddr()}},
			})
			if err != nil {
				return nil, err
			}
			a, err := mavlink.NewAdapter(link, cfg)
			if err != nil {
				_ = link.Close()
				return nil, err
			}
			return &mavlinkVector{Adapter: a, link: link}, nil
		},
	}, nil
}

// mavlinkVector closes its link with the adapter.
type mavlinkVector struct {
	*mavlink.Adapter
	link *mavlink.Link
}

func (v *mavlinkVector) Close() error {
	err := v.Adapter.Close()
	return errors.Join(err, v.link.Close())
}
