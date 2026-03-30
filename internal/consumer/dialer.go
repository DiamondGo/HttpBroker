package consumer

import (
	"context"
	"fmt"
	"net"

	"github.com/hashicorp/yamux"
	"go.uber.org/zap"
)

// NoopResolver implements go-socks5's NameResolver interface.
// It returns the hostname as-is without DNS lookup, ensuring domain names
// are passed through the tunnel to be resolved by the provider's DNS.
type NoopResolver struct{}

// Resolve returns the hostname without resolving it.
// The go-socks5 library calls this when a CONNECT request uses a domain name.
// We return nil IP so that AddrSpec.String() falls back to the FQDN, ensuring
// the original domain name is passed through the tunnel to be resolved by the
// provider's DNS — not replaced with 0.0.0.0.
func (r *NoopResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	return ctx, nil, nil
}

// tcpAddrConn wraps a net.Conn and overrides LocalAddr/RemoteAddr to return
// *net.TCPAddr. This is required because the go-socks5 library's SendReply
// function type-asserts the dialed connection's LocalAddr() to *net.TCPAddr.
// If the assertion fails (e.g., yamux streams return *yamuxAddr), the library
// silently replaces RepSuccess with RepAddrTypeNotSupported (SOCKS5 code 8),
// causing curl to report "cannot complete SOCKS5 connection".
type tcpAddrConn struct {
	net.Conn
}

// LocalAddr returns 0.0.0.0:0 as *net.TCPAddr so that the go-socks5 library's
// SendReply can type-assert it to *net.TCPAddr and produce a correctly sized
// 10-byte SOCKS5 success reply ([VER REP RSV ATYP A1 A2 A3 A4 P1 P2]).
// Returning &net.TCPAddr{} (nil IP) caused a 6-byte reply which blocked curl
// while it waited for the 4 missing IPv4 address bytes from the proxied stream.
func (c *tcpAddrConn) LocalAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4zero} }
func (c *tcpAddrConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

// TunnelDialer is a custom dialer for go-socks5.
// Each SOCKS5 CONNECT request opens a new yamux stream to the broker.
type TunnelDialer struct {
	sess   *yamux.Session
	logger *zap.Logger
}

// NewTunnelDialer creates a new TunnelDialer with the given yamux session and logger.
func NewTunnelDialer(sess *yamux.Session, logger *zap.Logger) *TunnelDialer {
	return &TunnelDialer{
		sess:   sess,
		logger: logger,
	}
}

// Dial opens a new yamux stream and writes the target address header.
// Called by go-socks5 for each CONNECT request.
// addr is "host:port" (may contain a domain name, not just IP).
//
// Target address format (must match broker's relay.go and provider's handler.go):
//   - Byte 0: uint8 = length of host:port string
//   - Bytes 1..N: host:port string (e.g., "example.com:443")
func (d *TunnelDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	// Validate address length fits in a single byte.
	if len(addr) > 255 {
		return nil, fmt.Errorf(
			"consumer: target address too long (%d bytes, max 255): %s",
			len(addr),
			addr,
		)
	}

	// Open a new yamux stream to the broker.
	stream, err := d.sess.Open()
	if err != nil {
		return nil, fmt.Errorf("consumer: failed to open yamux stream: %w", err)
	}

	// Write the target address header: [1 byte length][host:port string].
	header := make([]byte, 1+len(addr))
	header[0] = byte(len(addr))
	copy(header[1:], addr)

	if _, err := stream.Write(header); err != nil {
		stream.Close()
		return nil, fmt.Errorf("consumer: failed to write target address: %w", err)
	}

	d.logger.Debug("opened tunnel stream",
		zap.String("target", addr),
	)

	// Wrap the stream so LocalAddr()/RemoteAddr() return *net.TCPAddr.
	// The go-socks5 library's SendReply overwrites RepSuccess with
	// RepAddrTypeNotSupported if it cannot type-assert LocalAddr() to
	// *net.TCPAddr or *net.UDPAddr.
	return &tcpAddrConn{Conn: stream}, nil
}
