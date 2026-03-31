package provider

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kexiaowen/httpbroker/internal/transport"
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
	InsecureSkipVerify bool // Skip TLS certificate verification
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
		// Create HTTP transport with no timeouts (supports long-lived connections and large data transfers)
		httpTransport := &http.Transport{
			ResponseHeaderTimeout: 0, // no timeout waiting for response headers
			IdleConnTimeout:       0, // no timeout for idle connections
			DisableKeepAlives:     false,
		}
		if c.config.InsecureSkipVerify {
			httpTransport.TLSClientConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}
		httpClient := &http.Client{
			Timeout:   0, // no timeout for long-poll
			Transport: httpTransport,
		}

		connector := &transport.HTTPConnector{
			PollInterval: c.config.PollInterval,
			HTTPClient:   httpClient,
		}

		c.logger.Info("connecting to broker",
			zap.String("broker_url", c.config.BrokerURL),
			zap.String("endpoint", c.config.Endpoint),
		)

		conn, err := connector.Connect(c.config.BrokerURL, "provider", c.config.Endpoint)
		if err != nil {
			c.logger.Error("failed to connect to broker",
				zap.Error(err),
			)
			if !c.sleepOrDone(ctx, c.config.RetryBackoff) {
				return ctx.Err()
			}
			continue
		}

		// Step 2: Create yamux server session.
		// Disable keepalives: the HTTP polling transport cannot reliably complete
		// a PING-PONG round-trip within yamux's ConnectionWriteTimeout (10s).
		yamuxConfig := yamux.DefaultConfig()
		yamuxConfig.LogOutput = io.Discard
		yamuxConfig.EnableKeepAlive = false

		sess, err := yamux.Server(conn, yamuxConfig)
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

		c.logger.Info("connected to broker, accepting streams",
			zap.String("endpoint", c.config.Endpoint),
		)

		// Step 3: Accept streams in a loop.
		c.acceptStreams(ctx, sess)

		// Step 4: Close yamux session and retry.
		sess.Close()

		c.logger.Info("disconnected from broker, will reconnect",
			zap.String("endpoint", c.config.Endpoint),
		)

		if !c.sleepOrDone(ctx, c.config.RetryBackoff) {
			return ctx.Err()
		}
	}
}

// acceptStreams accepts yamux streams until an error occurs or ctx is cancelled.
func (c *Client) acceptStreams(ctx context.Context, sess *yamux.Session) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c.logger.Debug("waiting for stream (yamux accept)")
		stream, err := sess.Accept()
		if err != nil {
			if err != io.EOF {
				c.logger.Debug("yamux accept error",
					zap.Error(err),
				)
			}
			return
		}

		c.logger.Debug("stream accepted")
		go c.handler.Handle(stream)
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
