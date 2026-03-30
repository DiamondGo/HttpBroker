package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kexiaowen/httpbroker/internal/config"
	"github.com/kexiaowen/httpbroker/internal/consumer"
)

func main() {
	var configFile string
	var brokerURL string
	var endpoint string
	var socks5Listen string

	rootCmd := &cobra.Command{
		Use:   "httpbroker-consumer",
		Short: "HttpBroker - Consumer client (Machine B)",
		Long:  "Runs the consumer SOCKS5 proxy that tunnels browser traffic through the broker.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load config
			cfg, err := config.LoadConsumerConfig(configFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// CLI flag overrides
			if brokerURL != "" {
				cfg.Broker.URL = brokerURL
			}
			if endpoint != "" {
				cfg.Broker.Endpoint = endpoint
			}
			if socks5Listen != "" {
				cfg.Socks5.Listen = socks5Listen
			}

			// Apply defaults
			if cfg.Broker.URL == "" {
				cfg.Broker.URL = "http://localhost:8080"
			}
			if cfg.Broker.Endpoint == "" {
				cfg.Broker.Endpoint = "default"
			}
			if cfg.Socks5.Listen == "" {
				cfg.Socks5.Listen = ":1080"
			}
			if cfg.Transport.PollInterval == 0 {
				cfg.Transport.PollInterval = 50 * time.Millisecond
			}
			if cfg.Transport.PollTimeout == 0 {
				cfg.Transport.PollTimeout = 30 * time.Second
			}
			if cfg.Transport.RetryBackoff == 0 {
				cfg.Transport.RetryBackoff = 5 * time.Second
			}

			// Create logger
			logger, err := config.NewLogger(cfg.Logging.Level)
			if err != nil {
				return err
			}
			defer logger.Sync()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client := consumer.NewClient(consumer.Config{
				BrokerURL:    cfg.Broker.URL,
				Endpoint:     cfg.Broker.Endpoint,
				Socks5Listen: cfg.Socks5.Listen,
				PollInterval: cfg.Transport.PollInterval,
				PollTimeout:  cfg.Transport.PollTimeout,
				RetryBackoff: cfg.Transport.RetryBackoff,
			}, logger)

			logger.Info("starting consumer",
				zap.String("broker", cfg.Broker.URL),
				zap.String("endpoint", cfg.Broker.Endpoint),
				zap.String("socks5", cfg.Socks5.Listen))
			return client.Run(ctx)
		},
	}

	rootCmd.Flags().
		StringVarP(&configFile, "config", "c", "configs/consumer.yaml", "path to config file")
	rootCmd.Flags().
		StringVar(&brokerURL, "broker-url", "", "broker URL (e.g. http://192.168.1.100:8080)")
	rootCmd.Flags().StringVar(&endpoint, "endpoint", "", "endpoint name")
	rootCmd.Flags().
		StringVar(&socks5Listen, "socks5-listen", "", "SOCKS5 listen address (e.g. :1080)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
