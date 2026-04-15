package consumer

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"
	socks5 "github.com/things-go/go-socks5"
	"go.uber.org/zap"

	"github.com/DiamondGo/HttpBroker/internal/transport"
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
//   - An inner loop handles both broker and provider disconnections by
//     re-registering with the broker (/tunnel/connect) each time. This gives
//     a fresh HTTPConn and session ID, which is required because yamux frames
//     from the old session would corrupt a new yamux session on the same conn.
//   - Broker failures use exponential backoff (1 s → 3 min).
//   - Provider failures (broker closes yamux session) retry immediately with
//     a short 500 ms pause to avoid tight spin.
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

	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 3 * time.Minute
	)

	backoff := initialBackoff
	lastFailWasBroker := true // first attempt always treated as broker connect

	for {
		if err := ctx.Err(); err != nil {
			c.logger.Info("consumer shutting down",
				zap.String("endpoint", c.config.Endpoint),
			)
			return err
		}

		conn, err := c.connectToBroker()
		if err != nil {
			c.logger.Error("failed to connect to broker — will retry",
				zap.Error(err),
				zap.Duration("retry_in", backoff),
			)
			if !sleepOrDone(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			lastFailWasBroker = true
			continue
		}

		// Reset backoff on successful broker connection.
		if lastFailWasBroker {
			backoff = initialBackoff
		}
		lastFailWasBroker = false

		c.logger.Info("broker connection established",
			zap.String("endpoint", c.config.Endpoint),
		)

		// Build yamux session over the new broker connection.
		yamuxConfig := yamux.DefaultConfig()
		yamuxConfig.LogOutput = io.Discard
		yamuxConfig.EnableKeepAlive = false

		sess, err := yamux.Client(conn, yamuxConfig)
		if err != nil {
			c.logger.Error("failed to create yamux session — broker connection lost",
				zap.Error(err),
			)
			conn.Close()
			if !sleepOrDone(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			lastFailWasBroker = true
			continue
		}

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
		go c.serveLoop(serveCtx, socksServer, connQueue)

		// Wait for one of three events:
		//   1. ctx cancelled → clean exit
		//   2. conn.TransportFailed() → broker is gone, reconnect with backoff
		//   3. sess.CloseChan() → yamux session closed; check if broker is alive
		var providerDisconnected bool
		select {
		case <-ctx.Done():
			c.logger.Info("consumer shutting down — closing broker connection",
				zap.String("endpoint", c.config.Endpoint),
			)
			cancelServe()
			sess.Close()
			conn.Close()
			return ctx.Err()

		case <-conn.TransportFailed():
			c.logger.Warn("broker transport failed — will reconnect with backoff",
				zap.String("endpoint", c.config.Endpoint),
			)
			cancelServe()
			sess.Close()
			conn.Close()
			providerDisconnected = false
			lastFailWasBroker = true

		case <-sess.CloseChan():
			// yamux session closed. Check whether the HTTP transport is still alive.
			select {
			case <-conn.TransportFailed():
				c.logger.Warn(
					"broker transport failed (detected via yamux close) — will reconnect with backoff",
					zap.String("endpoint", c.config.Endpoint),
				)
				cancelServe()
				sess.Close()
				conn.Close()
				providerDisconnected = false
				lastFailWasBroker = true
			default:
				// Transport still alive — broker closed our yamux session because
				// the provider disconnected.
				c.logger.Warn("provider disconnected — re-registering with broker",
					zap.String("endpoint", c.config.Endpoint),
				)
				cancelServe()
				sess.Close()
				conn.Close()
				providerDisconnected = true
				lastFailWasBroker = false
			}
		}

		if providerDisconnected {
			// Brief pause before re-registering to avoid tight spin.
			if !sleepOrDone(ctx, 500*time.Millisecond) {
				return ctx.Err()
			}
			// backoff stays at initialBackoff for provider reconnects
		} else {
			// Broker failure — apply backoff.
			if !sleepOrDone(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
		}
	}
}

// connectToBroker creates a fresh HTTP client and registers with the broker.
func (c *Client) connectToBroker() (transport.Conn, error) {
	httpTransport := &http.Transport{
		ResponseHeaderTimeout: 0,
		IdleConnTimeout:       0,
		DisableKeepAlives:     false,
	}
	if c.config.InsecureSkipVerify {
		httpTransport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	httpClient := &http.Client{
		Timeout:   0,
		Transport: httpTransport,
	}

	connector := &transport.HTTPConnector{
		PollInterval: c.config.PollInterval,
		HTTPClient:   httpClient,
		AuthToken:    c.config.AuthToken,
	}

	return connector.Connect(c.config.BrokerURL, "consumer", c.config.Endpoint)
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

// sleepOrDone sleeps for d or returns false if ctx is done.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// minDuration returns the smaller of a and b.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
