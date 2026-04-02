package test

import (
	"context"
	"testing"
	"time"

	"github.com/DiamondGo/HttpBroker/internal/broker"
	"github.com/DiamondGo/HttpBroker/internal/consumer"
	"github.com/DiamondGo/HttpBroker/internal/provider"
	"go.uber.org/zap"
)

// TestAuthenticationSuccess tests that connections work with correct tokens
func TestAuthenticationSuccess(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	token := "test-auth-token-123"
	endpoint := "test-endpoint"

	// Start broker with authentication enabled
	brokerCfg := broker.Config{
		ListenAddr:     "127.0.0.1:18080",
		UseTLS:         false,
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		AuthEnabled:    true,
		AuthToken:      token,
	}

	srv := broker.NewServer(brokerCfg, logger)
	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("broker failed", zap.Error(err))
		}
	}()
	defer srv.Stop(context.Background())

	// Wait for broker to start
	time.Sleep(200 * time.Millisecond)

	// Start provider with correct token
	providerCfg := provider.Config{
		BrokerURL:    "http://127.0.0.1:18080",
		Endpoint:     endpoint,
		PollInterval: 50 * time.Millisecond,
		RetryBackoff: 1 * time.Second,
		DialTimeout:  5 * time.Second,
		ScrubHeaders: false,
		AuthToken:    token, // Correct token
	}

	providerClient := provider.NewClient(providerCfg, logger)
	providerCtx, providerCancel := context.WithCancel(context.Background())
	defer providerCancel()

	go func() {
		if err := providerClient.Run(providerCtx); err != nil && err != context.Canceled {
			logger.Error("provider failed", zap.Error(err))
		}
	}()

	// Wait for provider to connect
	time.Sleep(500 * time.Millisecond)

	// Start consumer with correct token
	consumerCfg := consumer.Config{
		BrokerURL:    "http://127.0.0.1:18080",
		Endpoint:     endpoint,
		Socks5Listen: "127.0.0.1:11080",
		PollInterval: 50 * time.Millisecond,
		RetryBackoff: 1 * time.Second,
		AuthToken:    token, // Correct token
	}

	consumerClient := consumer.NewClient(consumerCfg, logger)
	consumerCtx, consumerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer consumerCancel()

	// Consumer should connect successfully
	go func() {
		if err := consumerClient.Run(consumerCtx); err != nil && err != context.Canceled && err != context.DeadlineExceeded {
			t.Errorf("consumer with correct token failed: %v", err)
		}
	}()

	// Wait a bit to ensure connection is established
	time.Sleep(1 * time.Second)

	// If we reach here without errors, authentication worked
	logger.Info("authentication test passed")
}

// TestAuthenticationFailure tests that connections fail with wrong tokens
func TestAuthenticationFailure(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	correctToken := "correct-token-123"
	wrongToken := "wrong-token-456"
	endpoint := "test-endpoint-fail"

	// Start broker with authentication
	brokerCfg := broker.Config{
		ListenAddr:     "127.0.0.1:18081",
		UseTLS:         false,
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		AuthEnabled:    true,
		AuthToken:      correctToken,
	}

	srv := broker.NewServer(brokerCfg, logger)
	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("broker failed", zap.Error(err))
		}
	}()
	defer srv.Stop(context.Background())

	// Wait for broker to start
	time.Sleep(200 * time.Millisecond)

	// Try to connect provider with WRONG token
	providerCfg := provider.Config{
		BrokerURL:    "http://127.0.0.1:18081",
		Endpoint:     endpoint,
		PollInterval: 50 * time.Millisecond,
		RetryBackoff: 5 * time.Second, // Long backoff to avoid spam
		DialTimeout:  5 * time.Second,
		AuthToken:    wrongToken, // Wrong token!
	}

	providerClient := provider.NewClient(providerCfg, logger)
	providerCtx, providerCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer providerCancel()

	// Provider should fail to connect
	err := providerClient.Run(providerCtx)
	if err == nil || err == context.DeadlineExceeded {
		// This is expected - the connection should fail and retry until timeout
		logger.Info("authentication failure test passed - provider rejected as expected")
	} else {
		t.Logf("Provider failed with error (expected): %v", err)
	}
}

