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
	"github.com/kexiaowen/httpbroker/internal/provider"
)

func main() {
	var configFile string
	var brokerURL string
	var endpoint string
	var scrubHeaders bool
	var insecureSkipVerify bool

	rootCmd := &cobra.Command{
		Use:   "httpbroker-provider",
		Short: "HttpBroker - Provider client (Machine C)",
		Long:  "Runs the provider that dials target hosts and returns responses through the broker.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load config
			cfg, err := config.LoadProviderConfig(configFile)
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
			if cmd.Flags().Changed("scrub-headers") {
				cfg.Provider.ScrubHeaders = scrubHeaders
			}
			if cmd.Flags().Changed("insecure-skip-verify") {
				cfg.Broker.InsecureSkipVerify = insecureSkipVerify
			}

			// Apply defaults
			if cfg.Broker.URL == "" {
				cfg.Broker.URL = "http://localhost:8080"
			}
			if cfg.Broker.Endpoint == "" {
				cfg.Broker.Endpoint = "default"
			}
			if cfg.Provider.DialTimeout == 0 {
				cfg.Provider.DialTimeout = 10 * time.Second
			}
			if cfg.Transport.PollInterval == 0 {
				cfg.Transport.PollInterval = 50 * time.Millisecond
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

			client := provider.NewClient(provider.Config{
				BrokerURL:          cfg.Broker.URL,
				Endpoint:           cfg.Broker.Endpoint,
				PollInterval:       cfg.Transport.PollInterval,
				RetryBackoff:       cfg.Transport.RetryBackoff,
				DialTimeout:        cfg.Provider.DialTimeout,
				ScrubHeaders:       cfg.Provider.ScrubHeaders,
				InsecureSkipVerify: cfg.Broker.InsecureSkipVerify,
			}, logger)

			logger.Info("starting provider",
				zap.String("broker", cfg.Broker.URL),
				zap.String("endpoint", cfg.Broker.Endpoint))
			return client.Run(ctx)
		},
	}

	rootCmd.Flags().
		StringVarP(&configFile, "config", "c", "configs/provider.yaml", "path to config file")
	rootCmd.Flags().StringVar(&brokerURL, "broker-url", "", "broker URL")
	rootCmd.Flags().StringVar(&endpoint, "endpoint", "", "endpoint name")
	rootCmd.Flags().
		BoolVar(&scrubHeaders, "scrub-headers", false, "strip proxy headers from HTTP requests")
	rootCmd.Flags().
		BoolVar(&insecureSkipVerify, "insecure-skip-verify", false, "skip TLS certificate verification (for self-signed certs)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
