package provider

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/DiamondGo/HttpBroker/internal/transport"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"
)

// Config holds provider configuration.
type Config struct {
	BrokerURL          string
	Endpoint           string
	PollInterval       time.Duration
	RetryBackoff       time.Duration
	DialTimeout        time.Duration
	ScrubHeaders       bool
	InsecureSkipVerify bool   // Skip TLS certificate verification
	AuthToken          string // Authentication token for broker
}

// Client is the provider client.
type Client struct {
	config  Config
	handler *StreamHandler
	logger  *zap.Logger
}

// NewClient creates a new provider Client.
func NewClient(config Config, logger *zap.Logger) *Client {
	return &Client{
		config:  config,
		handler: NewStreamHandler(config.DialTimeout, config.ScrubHeaders, logger),
		logger:  logger,
	}
}

// Run connects to the broker and starts accepting streams.
// On broker disconnection it retries with exponential backoff (1 s → 3 min).
// Blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 3 * time.Minute
	)

	backoff := initialBackoff

	for {
		if err := ctx.Err(); err != nil {
			c.logger.Info("provider shutting down",
				zap.String("endpoint", c.config.Endpoint),
			)
			return err
		}

		// Build a fresh HTTP client for each connection attempt.
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

		c.logger.Info("connecting to broker",
			zap.String("broker_url", c.config.BrokerURL),
			zap.String("endpoint", c.config.Endpoint),
		)

		conn, err := connector.Connect(c.config.BrokerURL, "provider", c.config.Endpoint)
		if err != nil {
			c.logger.Error("failed to connect to broker — will retry",
				zap.Error(err),
				zap.Duration("retry_in", backoff),
			)
			if !sleepOrDone(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		// Connection established — reset backoff.
		backoff = initialBackoff
		c.logger.Info("connected to broker, accepting streams",
			zap.String("endpoint", c.config.Endpoint),
		)

		// Run the accept loop until the connection is lost or ctx is cancelled.
		c.runSession(ctx, conn)

		conn.Close()

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Connection lost — apply backoff before reconnecting.
		c.logger.Warn("broker connection lost — reconnecting with backoff",
			zap.Duration("retry_in", backoff),
		)
		if !sleepOrDone(ctx, backoff) {
			return ctx.Err()
		}
		backoff = minDuration(backoff*2, maxBackoff)
	}
}

// runSession creates a yamux server session over conn and accepts streams
// until the connection is lost or ctx is cancelled.
func (c *Client) runSession(ctx context.Context, conn transport.Conn) {
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.LogOutput = io.Discard
	yamuxConfig.EnableKeepAlive = false

	sess, err := yamux.Server(conn, yamuxConfig)
	if err != nil {
		c.logger.Error("failed to create yamux session",
			zap.Error(err),
		)
		return
	}
	defer sess.Close()

	// acceptStreams blocks until an error occurs or ctx is cancelled.
	c.acceptStreams(ctx, sess, conn)
}

// acceptStreams accepts yamux streams until an error occurs, the transport
// fails, or ctx is cancelled. Each accepted stream is handled in a goroutine.
func (c *Client) acceptStreams(ctx context.Context, sess *yamux.Session, conn transport.Conn) {
	// Run Accept in a goroutine so we can also watch ctx and TransportFailed.
	type acceptResult struct {
		stream net.Conn
		err    error
	}
	acceptCh := make(chan acceptResult, 1)

	go func() {
		for {
			stream, err := sess.Accept()
			acceptCh <- acceptResult{stream, err}
			if err != nil {
				return
			}
		}
	}()

	streamCount := 0
	for {
		select {
		case <-ctx.Done():
			return

		case <-conn.TransportFailed():
			c.logger.Warn("broker transport failed — stopping stream accept",
				zap.String("endpoint", c.config.Endpoint),
			)
			return

		case <-sess.CloseChan():
			c.logger.Warn("yamux session closed — stopping stream accept",
				zap.String("endpoint", c.config.Endpoint),
			)
			return

		case res := <-acceptCh:
			if res.err != nil {
				if res.err != io.EOF {
					c.logger.Debug("yamux accept error",
						zap.Error(res.err),
					)
				}
				return
			}

			streamCount++
			c.logger.Info("new consumer stream accepted",
				zap.String("endpoint", c.config.Endpoint),
				zap.Int("stream_count", streamCount),
			)
			go c.handler.Handle(res.stream)
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
