package config

import (
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// BrokerConfig holds all configuration for the broker server (Machine A).
type BrokerConfig struct {
	Server  ServerConfig  `mapstructure:"server"`
	Tunnel  TunnelConfig  `mapstructure:"tunnel"`
	Auth    AuthConfig    `mapstructure:"auth"`
	Logging LoggingConfig `mapstructure:"logging"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Listen string    `mapstructure:"listen"`
	TLS    TLSConfig `mapstructure:"tls"`
}

// TLSConfig holds TLS certificate paths.
type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

// TunnelConfig holds tunnel timing settings.
type TunnelConfig struct {
	PollTimeout    time.Duration `mapstructure:"poll_timeout"`
	SessionTimeout time.Duration `mapstructure:"session_timeout"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Token   string `mapstructure:"token"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level string `mapstructure:"level"` // "debug", "info", "warn", "error"
}

// ConsumerConfig holds configuration for the consumer (Machine B).
type ConsumerConfig struct {
	Broker    BrokerClientConfig `mapstructure:"broker"`
	Socks5    Socks5Config       `mapstructure:"socks5"`
	Transport TransportConfig    `mapstructure:"transport"`
	Logging   LoggingConfig      `mapstructure:"logging"`
}

// BrokerClientConfig holds broker connection settings for clients.
type BrokerClientConfig struct {
	URL                string `mapstructure:"url"`
	Endpoint           string `mapstructure:"endpoint"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"` // Skip TLS certificate verification (for self-signed certs)
	AuthToken          string `mapstructure:"auth_token"`           // Authentication token for broker
}

// Socks5Config holds SOCKS5 proxy settings.
type Socks5Config struct {
	Listen string `mapstructure:"listen"`
}

// TransportConfig holds transport timing settings.
type TransportConfig struct {
	PollInterval time.Duration `mapstructure:"poll_interval"`
	RetryBackoff time.Duration `mapstructure:"retry_backoff"`
}

// ProviderConfig holds configuration for the provider (Machine C).
type ProviderConfig struct {
	Broker    BrokerClientConfig `mapstructure:"broker"`
	Provider  ProviderOptions    `mapstructure:"provider"`
	Transport TransportConfig    `mapstructure:"transport"`
	Logging   LoggingConfig      `mapstructure:"logging"`
}

// ProviderOptions holds provider-specific settings.
type ProviderOptions struct {
	ScrubHeaders bool          `mapstructure:"scrub_headers"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
}

// LoadBrokerConfig loads broker config from a YAML file using viper.
func LoadBrokerConfig(path string) (*BrokerConfig, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	var cfg BrokerConfig
	if err := v.Unmarshal(&cfg, viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadConsumerConfig loads consumer config from a YAML file using viper.
func LoadConsumerConfig(path string) (*ConsumerConfig, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	var cfg ConsumerConfig
	if err := v.Unmarshal(&cfg, viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadProviderConfig loads provider config from a YAML file using viper.
func LoadProviderConfig(path string) (*ProviderConfig, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	var cfg ProviderConfig
	if err := v.Unmarshal(&cfg, viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// NewLogger creates a zap logger based on the log level string.
func NewLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		zapLevel = zapcore.InfoLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapLevel)
	return cfg.Build()
}
