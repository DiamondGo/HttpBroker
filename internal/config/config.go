package config

import (
	"errors"
	"fmt"
	"os"
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
	// EnableWebSocket lets the broker negotiate pollmux's WebSocket transport
	// for a session instead of poll/send-stream, when the connecting client
	// also asks for it (transport.prefer_websocket on the consumer/provider
	// side). Off by default: it's a new, additive transport that needs a
	// reverse proxy/CDN in front of the broker to forward WebSocket upgrade
	// requests on /tunnel/{id}/ws, so it must be opted into deliberately
	// rather than silently changed under existing deployments. When off (or
	// when the client doesn't ask), negotiation falls back to whatever
	// PollMode would otherwise select — this never breaks a client running
	// an older pollmux that doesn't know about WebSocket at all.
	EnableWebSocket bool `mapstructure:"enable_websocket"`
	// EnableResume lets the broker negotiate pollmux's resumable session for
	// a client that asks for it (transport.prefer_resume on the
	// consumer/provider side). A resumable session — and the yamux session
	// with every SSH/SOCKS stream inside it — survives its underlying
	// transport breaking: the broker keeps it for up to ResumeGrace after
	// the transport drops and accepts a POST /tunnel/{id}/resume that
	// re-attaches it, replaying whatever bytes the seam lost. This is what
	// lets long-lived streams outlive the ~1h "max connection age" cut that
	// CDNs/reverse proxies (observed with Cloudflare) apply regardless of
	// traffic. Off by default: purely additive, negotiation is "client
	// asks && server supports", and an older client that doesn't know
	// about it simply never asks. Only WebSocket or stream mode in both
	// directions can be resumed; a batch negotiation is never resumable.
	EnableResume bool `mapstructure:"enable_resume"`
	// ResumeGrace is how long a resumable session is kept after its
	// transport drops, waiting for the client's /resume. Zero/unset uses
	// pollmux's default (30s); pollmux caps it at 5m (the broker panics at
	// startup above that) because a detached session holds its replay
	// buffer for the whole grace.
	ResumeGrace time.Duration `mapstructure:"resume_grace"`
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
	// PreferWebSocket asks the broker to negotiate pollmux's WebSocket
	// transport instead of poll/send-stream. It only takes effect if the
	// broker also has tunnel.enable_websocket set — negotiation is "client
	// asks && server supports", so leaving this on is safe even against a
	// broker that hasn't turned WebSocket on (or an older broker that
	// doesn't know about it at all; it just silently falls back to
	// PollMode). WebSocket rides a single persistent connection for both
	// directions, avoiding the long-lived chunked-body buffering some CDNs
	// (observed with Cloudflare's standard tiers) apply to stream mode's
	// upload direction — see pollmux's README ("WebSocket 传输模式").
	PreferWebSocket bool `mapstructure:"prefer_websocket"`
	// PreferResume asks the broker to make this session resumable across
	// transport failures: instead of tearing down the yamux session (and
	// every stream in it) when the connection to the broker breaks, the
	// client resumes the same session and the streams carry on. It only
	// takes effect if the broker also has tunnel.enable_resume set —
	// negotiation is "client asks && server supports", so leaving this on
	// is safe against a broker that hasn't enabled it, or an older broker
	// that doesn't know about it at all (it silently falls back to today's
	// reconnect-with-a-new-session behaviour). Only honoured for WebSocket
	// transport or stream mode in both directions; with
	// upload_stream_preference on auto, a failed probe means the session
	// falls back to a non-resumable one. Both hops (consumer↔broker and
	// provider↔broker) must be resumable for a stream to survive either
	// side's transport breaking.
	PreferResume bool `mapstructure:"prefer_resume"`
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

// decodeHook converts YAML strings into time.Duration / []string fields.
func decodeHook() viper.DecoderConfigOption {
	return viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	))
}

// loadFromPath unmarshals the YAML file at path into dst (a pointer to a
// config struct). A missing file is NOT an error: every binary's CLI flags
// and built-in defaults are sufficient on their own, so running from a
// directory without a config file (e.g. an install dir or systemd unit with
// WorkingDirectory elsewhere) must keep working. A file that exists but fails
// to parse is still a hard error — that is almost certainly a typo the
// operator should see.
func loadFromPath(path string, dst any) error {
	v := viper.New()
	v.SetConfigFile(path)

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil // no config file: rely on CLI flags and defaults
	}

	if err := v.ReadInConfig(); err != nil {
		return err
	}
	return v.Unmarshal(dst, decodeHook())
}

// LoadBrokerConfig loads broker config from a YAML file using viper.
func LoadBrokerConfig(path string) (*BrokerConfig, error) {
	cfg := &BrokerConfig{}
	if err := loadFromPath(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConsumerConfig loads consumer config from a YAML file using viper.
func LoadConsumerConfig(path string) (*ConsumerConfig, error) {
	cfg := &ConsumerConfig{}
	if err := loadFromPath(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadProviderConfig loads provider config from a YAML file using viper.
func LoadProviderConfig(path string) (*ProviderConfig, error) {
	cfg := &ProviderConfig{}
	if err := loadFromPath(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NewLogger creates a zap logger based on the log level string.
// An empty level defaults to info; any other unrecognized level is an error
// (a typo like "verboase" should be visible, not silently downgraded).
func NewLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	if level == "" {
		zapLevel = zapcore.InfoLevel
	} else if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid logging.level %q (want debug, info, warn or error): %w", level, err)
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapLevel)
	return cfg.Build()
}
