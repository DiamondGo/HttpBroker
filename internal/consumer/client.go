package consumer

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"
	socks5 "github.com/things-go/go-socks5"
	"go.uber.org/zap"

	"github.com/kexiaowen/httpbroker/internal/transport"
)

// Config holds consumer configuration.
type Config struct {
	BrokerURL    string
	Endpoint     string
	Socks5Listen string // e.g. ":1080"
	PollInterval time.Duration
	RetryBackoff time.Duration
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

// Run connects to the broker, sets up a SOCKS5 server, and starts serving.
// Reconnects automatically on failure. Blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	for {
		// Check if context is cancelled before attempting connection.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Step 1: Connect to broker.
		connector := &transport.HTTPConnector{
			PollInterval: c.config.PollInterval,
			HTTPClient:   &http.Client{Timeout: 0}, // no timeout for long-poll
		}

		c.logger.Info("connecting to broker",
			zap.String("broker_url", c.config.BrokerURL),
			zap.String("endpoint", c.config.Endpoint),
		)

		conn, err := connector.Connect(c.config.BrokerURL, "consumer", c.config.Endpoint)
		if err != nil {
			c.logger.Error("failed to connect to broker",
				zap.Error(err),
			)
			if !c.sleepOrDone(ctx, c.config.RetryBackoff) {
				return ctx.Err()
			}
			continue
		}

		// Step 2: Create yamux CLIENT session (consumer opens streams to broker).
		// Disable keepalives: the HTTP polling transport cannot reliably complete
		// a PING-PONG round-trip within yamux's ConnectionWriteTimeout (10s).
		yamuxConfig := yamux.DefaultConfig()
		yamuxConfig.LogOutput = io.Discard
		yamuxConfig.EnableKeepAlive = false

		sess, err := yamux.Client(conn, yamuxConfig)
		if err != nil {
			c.logger.Error("failed to create yamux session",
				zap.Error(err),
			)
			conn.Close()
			if !c.sleepOrDone(ctx, c.config.RetryBackoff) {
				return ctx.Err()
			}
			continue
		}

		c.logger.Info("connected to broker, starting SOCKS5 server",
			zap.String("endpoint", c.config.Endpoint),
			zap.String("listen", c.config.Socks5Listen),
		)

		// Step 3: Create SOCKS5 server with custom dialer and NoopResolver.
		dialer := NewTunnelDialer(sess, c.logger)

		socksServer := socks5.NewServer(
			socks5.WithDial(dialer.Dial),
			socks5.WithResolver(&NoopResolver{}),
		)

		// Step 4: Create TCP listener for SOCKS5.
		listener, err := net.Listen("tcp", c.config.Socks5Listen)
		if err != nil {
			c.logger.Error("failed to listen for SOCKS5",
				zap.String("listen", c.config.Socks5Listen),
				zap.Error(err),
			)
			sess.Close()
			if !c.sleepOrDone(ctx, c.config.RetryBackoff) {
				return ctx.Err()
			}
			continue
		}

		// Step 5: Serve SOCKS5 connections until yamux session closes or ctx is cancelled.
		c.serveSocks5(ctx, socksServer, listener, sess)

		// Step 6: Clean up and reconnect.
		sess.Close()

		c.logger.Info("disconnected from broker, will reconnect",
			zap.String("endpoint", c.config.Endpoint),
		)

		if !c.sleepOrDone(ctx, c.config.RetryBackoff) {
			return ctx.Err()
		}
	}
}

// serveSocks5 runs the SOCKS5 server until the yamux session closes or ctx is cancelled.
func (c *Client) serveSocks5(
	ctx context.Context,
	server *socks5.Server,
	listener net.Listener,
	sess *yamux.Session,
) {
	// Run SOCKS5 server in a goroutine. Serve blocks until listener is closed.
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	// Wait for yamux session close, context cancellation, or serve error.
	select {
	case <-ctx.Done():
		c.logger.Info("context cancelled, stopping SOCKS5 server")
		listener.Close()
		<-serveDone
	case <-sess.CloseChan():
		c.logger.Info("yamux session closed, stopping SOCKS5 server")
		listener.Close()
		<-serveDone
	case err := <-serveDone:
		if err != nil {
			c.logger.Error("SOCKS5 server error",
				zap.Error(err),
			)
		}
	}
}

// sleepOrDone sleeps for the given duration or returns false if ctx is done.
func (c *Client) sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
