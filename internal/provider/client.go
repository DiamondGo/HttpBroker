package provider

import (
	"context"
	"log/slog"
	"time"

	"github.com/DiamondGo/pollmux"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
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
	// CoalesceWindow is passed through to pollmux.Connector; <= 0 uses
	// pollmux.DefaultCoalesceWindow.
	CoalesceWindow time.Duration
	// PollMode is "" or "stream" (default) to request stream mode, or
	// "batch" to opt out and force the older discrete poll mode.
	PollMode string
	// PollGrace is passed through to pollmux.Connector; <= 0 uses
	// pollmux.DefaultPollGrace. Raise it when the broker sits behind a
	// reverse proxy/CDN whose own latency can push a stream-mode poll's
	// response headers past the default 10s.
	PollGrace time.Duration
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

// preferStreamMode reports whether pollMode should make the client ask the
// broker to negotiate stream mode. Empty (default) prefers it; only "batch"
// opts out. The broker independently decides too — negotiation is "client
// asks && server supports" — so this is safe to leave at the default even
// against an older broker.
func preferStreamMode(pollMode string) bool {
	return pollMode != pollmux.PollModeBatch
}

// Run connects to the broker and accepts streams, reconnecting with backoff
// on failure. Blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	slogger := slog.New(zapslog.NewHandler(c.logger.Core()))

	loop := &pollmux.ReconnectLoop{
		Connect: func(ctx context.Context) (pollmux.Conn, error) {
			c.logger.Info("connecting to broker",
				zap.String("broker_url", c.config.BrokerURL),
				zap.String("endpoint", c.config.Endpoint),
			)
			connector := &pollmux.Connector{
				BaseURL:   c.config.BrokerURL,
				AuthToken: c.config.AuthToken,
				Meta: map[string]string{
					"role":     "provider",
					"endpoint": c.config.Endpoint,
				},
				PollInterval:       c.config.PollInterval,
				CoalesceWindow:     c.config.CoalesceWindow,
				InsecureSkipVerify: c.config.InsecureSkipVerify,
				PreferStream:       preferStreamMode(c.config.PollMode),
				PollGrace:          c.config.PollGrace,
				Logger:             slogger,
			}
			return connector.Connect(ctx)
		},
		Serve: func(ctx context.Context, conn pollmux.Conn) pollmux.Outcome {
			// Provider connection: broker opens streams TO the provider, so
			// the provider is the yamux server (accepts streams).
			sess, err := pollmux.ServerSession(conn)
			if err != nil {
				c.logger.Error("failed to create yamux session", zap.Error(err))
				return pollmux.OutcomeTransportFailed
			}
			defer sess.Close()

			c.logger.Info("connected to broker, accepting streams",
				zap.String("endpoint", c.config.Endpoint),
			)
			return pollmux.AcceptLoop(ctx, sess, conn, c.handler.Handle)
		},
		InitialBackoff: c.config.RetryBackoff,
		Logger:         slogger,
	}

	err := loop.Run(ctx)
	if ctx.Err() != nil {
		c.logger.Info("provider shutting down", zap.String("endpoint", c.config.Endpoint))
	}
	return err
}
