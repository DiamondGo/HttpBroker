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
	Listen                string                     `mapstructure:"listen"`
	TLS                   TLSConfig                  `mapstructure:"tls"`
	StatusEndpointEnabled bool                       `mapstructure:"status_endpoint_enabled"` // Whether to expose GET /status endpoint (default: false)
	UnauthorizedRedirect  UnauthorizedRedirectConfig `mapstructure:"unauthorized_redirect"`   // Redirect settings for unauthorized requests
}

// TLSConfig holds TLS certificate paths.
type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

// UnauthorizedRedirectConfig holds redirect settings for unauthorized requests.
type UnauthorizedRedirectConfig struct {
	Enabled bool   `mapstructure:"enabled"` // Whether to redirect unauthorized requests instead of returning 401/404
	URL     string `mapstructure:"url"`     // Redirect target URL (supports: "/path", "www.example.com", or "https://example.com")
}

// TunnelConfig holds tunnel timing settings.
type TunnelConfig struct {
	PollTimeout    time.Duration `mapstructure:"poll_timeout"`
	SessionTimeout time.Duration `mapstructure:"session_timeout"`
	// CoalesceWindow bounds how long the broker waits for more data to
	// accumulate on a long-poll response once at least one byte is
	// available, instead of flushing immediately. Zero/unset uses
	// pollmux.DefaultCoalesceWindow (2ms).
	CoalesceWindow time.Duration `mapstructure:"coalesce_window"`
	// PollBufferSize caps how many bytes one poll response may carry.
	// Zero/unset uses pollmux.DefaultPollBufferSize (256KiB).
	PollBufferSize int `mapstructure:"poll_buffer_size"`
	// MaxSendBytes caps a single request body. Zero/unset uses
	// pollmux.DefaultMaxSendBytes (1MiB).
	MaxSendBytes int `mapstructure:"max_send_bytes"`
	// HighWaterWarn logs a one-shot warning when either direction of a
	// session buffers this many bytes. Zero disables it.
	HighWaterWarn int `mapstructure:"high_water_warn"`
	// PollMode selects the downstream transport mode. Empty/unset defaults
	// to "stream": the long-poll response stays open and pushes data as it
	// arrives, removing batch mode's one-poll-buffer-per-RTT ceiling. Set
	// to "batch" to force the older discrete request/response mode. Any
	// other value panics the broker at startup rather than silently
	// defaulting.
	PollMode string `mapstructure:"poll_mode"`
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
	// CoalesceWindow bounds how long pollmux.Connector's Write() waits before
	// the first send of an idle-to-active burst, giving immediately-following
	// writes a chance to merge into the same HTTP request. Zero/unset uses
	// pollmux.DefaultCoalesceWindow (2ms).
	CoalesceWindow time.Duration `mapstructure:"coalesce_window"`
	// PollMode requests the server negotiate stream mode. Empty/unset
	// defaults to "stream"; set to "batch" to opt out. Negotiation still
	// requires the broker to support stream mode too, so this is safe to
	// leave at the default even against an older broker.
	PollMode string `mapstructure:"poll_mode"`
	// PollGrace is how long the poll request's ResponseHeaderTimeout waits
	// beyond the point pollmux itself would normally have replied, before
	// giving up on that HTTP round trip. In stream mode this is effectively
	// the whole timeout (the broker flushes headers almost immediately once
	// it replies at all), so a reverse proxy, CDN, or load balancer sitting
	// in front of the broker that adds its own connection/header latency
	// needs this raised — the default (pollmux.DefaultPollGrace, 10s) can be
	// too tight for that extra hop. Zero/unset uses the pollmux default.
	PollGrace time.Duration `mapstructure:"poll_grace"`
	// UploadStreamPreference controls the upload direction (this client ->
	// broker) independently of PollMode, which only governs the download
	// direction: "" (default) auto-detects once at connect time via a probe
	// bounded by UploadProbeTimeout; "stream" always uses upload streaming
	// once the broker offers it, skipping the probe — only safe once you've
	// separately confirmed every hop between this client and the broker
	// forwards a long-lived chunked request body live; "batch" never uses
	// it. Auto-detect exists because some intermediate proxies (observed
	// with Cloudflare's standard tiers in production) buffer a long-lived
	// chunked request body instead of forwarding it live, which hangs the
	// tunnel outright rather than just being slow — see pollmux's README
	// ("连接期自动探测") for the full story.
	UploadStreamPreference string `mapstructure:"upload_stream_preference"`
	// UploadProbeTimeout bounds the auto-detect probe run when
	// UploadStreamPreference is "". Zero/unset uses pollmux's default (15s).
	UploadProbeTimeout time.Duration `mapstructure:"upload_probe_timeout"`
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
