package consumer

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/DiamondGo/pollmux"
	socks5 "github.com/things-go/go-socks5"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
)

// Config holds consumer configuration.
type Config struct {
	BrokerURL          string
	Endpoint           string
	Socks5Listen       string // e.g. ":1080"
	PollInterval       time.Duration
	RetryBackoff       time.Duration
	InsecureSkipVerify bool   // Skip TLS certificate verification
	AuthToken          string // Authentication token for broker
	// CoalesceWindow is passed through to pollmux.Connector; <= 0 uses
	// pollmux.DefaultCoalesceWindow.
	CoalesceWindow time.Duration
}

// Client is the consumer client.
type Client struct {
	config Config
	logger *zap.Logger
}

// NewClient creates a new consumer Client.
func NewClient(config Config, logger *zap.Logger) *Client {
	return &Client{
		config: config,
		logger: logger,
	}
}

// Run is the main reconnect loop.
//
// Architecture:
//   - A single persistent SOCKS5 TCP listener is created once and kept alive
//     for the entire lifetime of the process. This means the SOCKS5 port is
//     always available to the browser, even during reconnects.
//   - pollmux.ReconnectLoop re-registers with the broker (/tunnel/connect)
//     each time, giving a fresh session. Consumer is the stream-opening end,
//     not the stream-accepting end, so AcceptLoop doesn't apply here — Serve
//     is a hand-written four-way select, same shape as before migration.
func (c *Client) Run(ctx context.Context) error {
	// Create the SOCKS5 listener once. It survives all reconnects.
	listener, err := net.Listen("tcp", c.config.Socks5Listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	c.logger.Info("SOCKS5 listener started",
		zap.String("listen", c.config.Socks5Listen),
	)

	// connQueue receives accepted TCP connections from the acceptLoop goroutine.
	connQueue := make(chan net.Conn, 64)

	acceptCtx, cancelAccept := context.WithCancel(ctx)
	defer cancelAccept()
	go c.acceptLoop(acceptCtx, listener, connQueue)

	slogger := slog.New(zapslog.NewHandler(c.logger.Core()))

	loop := &pollmux.ReconnectLoop{
		Connect: func(ctx context.Context) (pollmux.Conn, error) {
			connector := &pollmux.Connector{
				BaseURL:   c.config.BrokerURL,
				AuthToken: c.config.AuthToken,
				Meta: map[string]string{
					"role":     "consumer",
					"endpoint": c.config.Endpoint,
				},
				PollInterval:       c.config.PollInterval,
				CoalesceWindow:     c.config.CoalesceWindow,
				InsecureSkipVerify: c.config.InsecureSkipVerify,
				Logger:             slogger,
			}
			c.logger.Info("connecting to broker",
				zap.String("broker_url", c.config.BrokerURL),
				zap.String("endpoint", c.config.Endpoint),
			)
			return connector.Connect(ctx)
		},

		Serve: func(ctx context.Context, conn pollmux.Conn) pollmux.Outcome {
			// Consumer connection: the consumer opens streams TO the broker,
			// so it is the yamux client.
			sess, err := pollmux.ClientSession(conn)
			if err != nil {
				c.logger.Error("failed to create yamux session — broker connection lost",
					zap.Error(err),
				)
				return pollmux.OutcomeTransportFailed
			}
			defer sess.Close()

			c.logger.Info("yamux session established, ready to serve SOCKS5 traffic",
				zap.String("endpoint", c.config.Endpoint),
			)

			dialer := NewTunnelDialer(sess, c.logger)
			socksServer := socks5.NewServer(
				socks5.WithDial(dialer.Dial),
				socks5.WithResolver(&NoopResolver{}),
			)

			// serveCtx is cancelled when this yamux session ends.
			serveCtx, cancelServe := context.WithCancel(ctx)
			defer cancelServe()
			go c.serveLoop(serveCtx, socksServer, connQueue)

			select {
			case <-ctx.Done():
				c.logger.Info("consumer shutting down — closing broker connection",
					zap.String("endpoint", c.config.Endpoint),
				)
				return pollmux.OutcomeShutdown

			case <-conn.TransportFailed():
				c.logger.Warn("broker transport failed — will reconnect with backoff",
					zap.String("endpoint", c.config.Endpoint),
				)
				return pollmux.OutcomeTransportFailed

			case <-sess.CloseChan():
				// yamux session closed. Check whether the HTTP transport is
				// still alive to tell "broker gone" from "provider left,
				// broker closed our session" — a yamux close alone can't
				// distinguish the two.
				select {
				case <-conn.TransportFailed():
					c.logger.Warn(
						"broker transport failed (detected via yamux close) — will reconnect with backoff",
						zap.String("endpoint", c.config.Endpoint),
					)
					return pollmux.OutcomeTransportFailed
				default:
					c.logger.Warn("provider disconnected — re-registering with broker",
						zap.String("endpoint", c.config.Endpoint),
					)
					return pollmux.OutcomePeerClosed
				}
			}
		},

		// InitialBackoff is operator-configurable via RetryBackoff (unset uses
		// pollmux.DefaultInitialBackoff, 1s).
		//
		// Note this now governs the provider-disconnect reconnect delay too:
		// when the provider leaves, the broker closes the consumer's tunnel as
		// well (yamux.Session.Close closes the underlying conn), so the
		// consumer sees 410/TransportFailed() instead of the pre-migration
		// 204/PeerClosed path, and reconnects through the backoff branch
		// rather than PeerClosedPause. Since ReconnectLoop resets backoff on
		// every successful connect, a low RetryBackoff keeps that case's
		// delay small and constant; a higher value trades slower recovery
		// from a flapping provider for less connection churn.
		InitialBackoff: c.config.RetryBackoff,
		Logger:         slogger,
	}

	err = loop.Run(ctx)
	if ctx.Err() != nil {
		c.logger.Info("consumer shutting down", zap.String("endpoint", c.config.Endpoint))
	}
	return err
}

// acceptLoop continuously accepts TCP connections from listener and sends them
// to connQueue. It stops when acceptCtx is cancelled or listener is closed.
func (c *Client) acceptLoop(ctx context.Context, listener net.Listener, connQueue chan<- net.Conn) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // context cancelled, normal exit
			}
			c.logger.Debug("SOCKS5 accept error", zap.Error(err))
			return
		}

		select {
		case connQueue <- conn:
		case <-ctx.Done():
			conn.Close()
			return
		default:
			// Queue full — drop connection to avoid blocking.
			c.logger.Warn("SOCKS5 connection queue full, dropping connection")
			conn.Close()
		}
	}
}

// serveLoop reads connections from connQueue and serves each one through the
// SOCKS5 server in a separate goroutine. It stops when serveCtx is cancelled.
func (c *Client) serveLoop(ctx context.Context, server *socks5.Server, connQueue <-chan net.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case conn, ok := <-connQueue:
			if !ok {
				return
			}
			go func(nc net.Conn) {
				if err := server.ServeConn(nc); err != nil {
					c.logger.Debug("SOCKS5 serve conn error", zap.Error(err))
				}
			}(conn)
		}
	}
}
