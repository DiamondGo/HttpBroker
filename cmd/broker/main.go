package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/DiamondGo/HttpBroker/internal/broker"
	"github.com/DiamondGo/HttpBroker/internal/config"
)

var (
	version = "dev" // Version number, set at build time
)

func main() {
	var configFile string
	var listenAddr string
	var tlsCert string
	var tlsKey string
	var enableStatusEndpoint bool

	rootCmd := &cobra.Command{
		Use:   "httpbroker-broker",
		Short: "HttpBroker - Broker server (Machine A)",
		Long:  "Runs the broker server that relays traffic between consumers and providers.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load config
			cfg, err := config.LoadBrokerConfig(configFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// CLI flag overrides
			if listenAddr != "" {
				cfg.Server.Listen = listenAddr
			}
			if tlsCert != "" {
				cfg.Server.TLS.CertFile = tlsCert
				cfg.Server.TLS.Enabled = true
			}
			if tlsKey != "" {
				cfg.Server.TLS.KeyFile = tlsKey
				cfg.Server.TLS.Enabled = true
			}
			if enableStatusEndpoint {
				cfg.Server.StatusEndpointEnabled = true
			}

			// Apply defaults
			if cfg.Server.Listen == "" {
				cfg.Server.Listen = ":8080"
			}
			if cfg.Tunnel.PollTimeout == 0 {
				// 1s allows yamux frames (including keepalive PING-PONG) to be
				// delivered in time. A 30s long-poll would block the HTTPConn poll
				// loop, preventing pending yamux frames in writeBuf from being sent
				// for up to 30s — causing curl timeouts and yamux keepalive failures.
				cfg.Tunnel.PollTimeout = 1 * time.Second
			}
			if cfg.Tunnel.SessionTimeout == 0 {
				cfg.Tunnel.SessionTimeout = 5 * time.Minute
			}

			// Create logger
			logger, err := config.NewLogger(cfg.Logging.Level)
			if err != nil {
				return err
			}
			defer logger.Sync()

			// Build broker config
			brokerCfg := broker.Config{
				ListenAddr:            cfg.Server.Listen,
				UseTLS:                cfg.Server.TLS.Enabled,
				TLSCertFile:           cfg.Server.TLS.CertFile,
				TLSKeyFile:            cfg.Server.TLS.KeyFile,
				PollTimeout:           cfg.Tunnel.PollTimeout,
				SessionTimeout:        cfg.Tunnel.SessionTimeout,
				AuthEnabled:           cfg.Auth.Enabled,
				AuthToken:             cfg.Auth.Token,
				StatusEndpointEnabled: cfg.Server.StatusEndpointEnabled,
				Version:               version, // Pass version to broker config
			}

			// Create and start server
			srv := broker.NewServer(brokerCfg, logger)

			// Handle graceful shutdown
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				srv.Stop(shutdownCtx)
			}()

			logger.Info("starting broker", zap.String("listen", brokerCfg.ListenAddr))
			return srv.Start()
		},
	}

	rootCmd.Flags().
		StringVarP(&configFile, "config", "c", "configs/broker.yaml", "path to config file")
	rootCmd.Flags().StringVar(&listenAddr, "listen", "", "override listen address (e.g. :8080)")
	rootCmd.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate file")
	rootCmd.Flags().StringVar(&tlsKey, "tls-key", "", "TLS key file")
	rootCmd.Flags().BoolVar(&enableStatusEndpoint, "enable-status", false, "enable GET /status endpoint")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
