# HttpBroker

A three-node proxy network that lets machines access network resources through an intermediary provider, using standard HTTP/HTTPS traffic. All tunnel traffic looks like ordinary web API calls.

## Overview

HttpBroker creates a TCP tunnel across three machines:

- **Machine A (Broker)** — a central relay server accessible to both B and C
- **Machine B (Consumer)** — runs a local SOCKS5 proxy; your browser connects here
- **Machine C (Provider)** — dials target hosts on its local network and relays data back

```
┌─────────────┐            ┌─────────────┐            ┌─────────────┐
│  Machine B  │            │  Machine A  │            │  Machine C  │
│  (Consumer) │◄──HTTP/S──►│  (Broker)   │◄──HTTP/S──►│  (Provider) │
│             │            │             │            │             │
│ SOCKS5 :1080│            │ HTTP/S      │            │ Dials target│
│ Browser ──► │ ─────────► │ :8080       │ ─────────► │ host:port   │
│             │ ◄───────── │             │ ◄───────── │             │
└─────────────┘            └─────────────┘            └─────────────┘
```

**Traffic flow:**

```
Browser → SOCKS5 (B:1080) → Broker (A:8080) → Provider (C) → Target Website
```

DNS is resolved on Machine C (the Provider), so VPN-internal or private hostnames work correctly.

## How It Works

### Long-Polling Transport

Both the Consumer and Provider maintain a continuous loop of HTTP POST requests to the Broker:

1. **POST body** carries upstream data (from client to broker)
2. **Response body** carries downstream data (from broker to client)
3. The Broker holds the response open (long-poll) until data is available or a timeout (30s) expires
4. The client immediately sends the next POST after receiving a response

To any network observer, this looks like a web application making regular API calls — no WebSockets, no persistent connections, no special protocols.

### yamux Multiplexing

Multiple browser connections (tabs, concurrent requests) are multiplexed over a single logical HTTP session using [hashicorp/yamux](https://github.com/hashicorp/yamux). Each browser connection becomes a yamux stream, all sharing the same poll loop.

### SOCKS5 Proxy

The Consumer runs a local SOCKS5 server. When your browser makes a request through the SOCKS5 proxy, the Consumer opens a new yamux stream, sends the CONNECT request through the tunnel, and the Provider dials the target host on its network.

### Header Scrubbing

The Provider can optionally strip proxy-revealing headers (`X-Forwarded-For`, `Via`, `Proxy-Authorization`, etc.) from HTTP requests before forwarding them.

## Quick Start

### Prerequisites

- **Go 1.25+**
- Three machines (or three terminal windows on one machine for testing)

### Build

```bash
# Build all three binaries for the current platform
make build-all

# Cross-compile the broker for Raspberry Pi (arm64)
make build-pi

# Cross-compile the broker for older Raspberry Pi (armv7)
make build-pi-armv7

# Cross-compile all binaries for linux/amd64 (VPS/server)
make build-linux

# Build with version info
make build-release VERSION=v1.0.0
```

Binaries are placed in the `bin/` directory.

### Machine A — Start the Broker

```bash
# Using config file (default: configs/broker.yaml)
./bin/httpbroker-broker --config configs/broker.yaml

# Or with CLI flags
./bin/httpbroker-broker --listen :8080

# With TLS
./bin/httpbroker-broker --listen :8443 --tls-cert server.crt --tls-key server.key
```

The broker listens on `:8080` by default and waits for Consumer and Provider connections.

**Health check:**

```bash
curl http://BROKER_IP:8080/status
```

### Machine B — Start the Consumer

```bash
# Using config file
./bin/httpbroker-consumer --config configs/consumer.yaml

# Or with CLI flags
./bin/httpbroker-consumer --broker-url http://BROKER_IP:8080 --endpoint vpn1 --socks5-listen :1080
```

This starts a SOCKS5 proxy on `127.0.0.1:1080`. Point your browser at this address (see [Browser Configuration](#browser-configuration) below).

### Machine C — Start the Provider

```bash
# Using config file
./bin/httpbroker-provider --config configs/provider.yaml

# Or with CLI flags
./bin/httpbroker-provider --broker-url http://BROKER_IP:8080 --endpoint vpn1 --scrub-headers
```

The Provider connects to the Broker and waits for tunnel requests. It dials target hosts on its local network and relays traffic back through the Broker.

### Test with curl

Once all three nodes are running:

```bash
curl --socks5-hostname 127.0.0.1:1080 http://example.com
```

## Browser Configuration

**Important:** You must enable remote DNS resolution so that domain names are resolved on the Provider (Machine C), not on your local machine.

### Firefox

1. Open **Settings** → **General** → scroll to **Network Settings** → click **Settings…**
2. Select **Manual proxy configuration**
3. Set **SOCKS Host**: `127.0.0.1`, **Port**: `1080`
4. Select **SOCKS v5**
5. ✅ Check **Proxy DNS when using SOCKS v5**
6. Click **OK**

### Chrome / Chromium

Launch Chrome with proxy flags:

```bash
google-chrome \
  --proxy-server="socks5://127.0.0.1:1080" \
  --host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE 127.0.0.1"
```

The `--host-resolver-rules` flag forces DNS resolution through the SOCKS5 proxy.

### curl

```bash
# --socks5-hostname resolves DNS on the remote side (Provider)
curl --socks5-hostname 127.0.0.1:1080 http://example.com

# HTTPS works too
curl --socks5-hostname 127.0.0.1:1080 https://example.com
```

### System-wide (macOS)

1. Open **System Preferences** → **Network** → select your connection → **Advanced** → **Proxies**
2. Enable **SOCKS Proxy**
3. Set server to `127.0.0.1` and port to `1080`

## Configuration

Each binary reads a YAML config file and supports CLI flag overrides. CLI flags take precedence over config file values.

### Broker (`configs/broker.yaml`)

```yaml
server:
  listen: ":8080"          # Address to listen on
  tls:
    enabled: false         # Enable TLS
    cert_file: ""          # Path to TLS certificate
    key_file: ""           # Path to TLS private key

tunnel:
  poll_timeout: "5s"       # How long to hold a poll request before returning empty
  session_timeout: "5m"    # Disconnect sessions idle longer than this

auth:
  enabled: false           # Authentication (placeholder for future use)

logging:
  level: "info"            # Log level: debug, info, warn, error
```

### Consumer (`configs/consumer.yaml`)

```yaml
broker:
  url: "http://127.0.0.1:8080"   # Broker URL
  endpoint: "default"             # Endpoint name (must match Provider)

socks5:
  listen: ":1080"                 # Local SOCKS5 listen address

transport:
  poll_interval: "50ms"           # Delay between poll requests
  retry_backoff: "5s"             # Wait time before reconnecting on error

logging:
  level: "info"                   # Log level: debug, info, warn, error
```

### Provider (`configs/provider.yaml`)

```yaml
broker:
  url: "http://127.0.0.1:8080"   # Broker URL
  endpoint: "default"             # Endpoint name (must match Consumer)

provider:
  scrub_headers: true             # Strip proxy-revealing HTTP headers
  dial_timeout: "10s"             # Timeout when dialing target hosts

transport:
  poll_interval: "50ms"           # Delay between poll requests
  retry_backoff: "5s"             # Wait time before reconnecting on error

logging:
  level: "info"                   # Log level: debug, info, warn, error
```

### Multiple Endpoints

You can run multiple Providers on different endpoints to access different networks:

```yaml
# Provider 1 — configs/provider-vpn1.yaml
broker:
  url: "http://BROKER_IP:8080"
  endpoint: vpn1

# Provider 2 — configs/provider-office.yaml
broker:
  url: "http://BROKER_IP:8080"
  endpoint: office
```

Then start separate Consumers for each:

```bash
# Consumer connecting to vpn1
./bin/httpbroker-consumer --broker-url http://BROKER_IP:8080 --endpoint vpn1 --socks5-listen :1080

# Consumer connecting to office
./bin/httpbroker-consumer --broker-url http://BROKER_IP:8080 --endpoint office --socks5-listen :1081
```

Each Consumer gets its own SOCKS5 port, and you can configure different browsers or profiles to use different proxies.

## CLI Reference

### httpbroker-broker

```
Runs the broker server that relays traffic between consumers and providers.

Usage:
  httpbroker-broker [flags]

Flags:
  -c, --config string    path to config file (default "configs/broker.yaml")
      --listen string    override listen address (e.g. :8080)
      --tls-cert string  TLS certificate file
      --tls-key string   TLS key file
  -h, --help             help for httpbroker-broker
```

### httpbroker-consumer

```
Runs the consumer SOCKS5 proxy that tunnels browser traffic through the broker.

Usage:
  httpbroker-consumer [flags]

Flags:
  -c, --config string         path to config file (default "configs/consumer.yaml")
      --broker-url string     broker URL (e.g. http://192.168.1.100:8080)
      --endpoint string       endpoint name
      --socks5-listen string  SOCKS5 listen address (e.g. :1080)
  -h, --help                  help for httpbroker-consumer
```

### httpbroker-provider

```
Runs the provider that dials target hosts and returns responses through the broker.

Usage:
  httpbroker-provider [flags]

Flags:
  -c, --config string      path to config file (default "configs/provider.yaml")
      --broker-url string  broker URL
      --endpoint string    endpoint name
      --scrub-headers      strip proxy headers from HTTP requests (default false)
  -h, --help               help for httpbroker-provider
```

## Architecture

The project follows a clean Go package structure:

```
cmd/
  broker/       → httpbroker-broker binary
  consumer/     → httpbroker-consumer binary
  provider/     → httpbroker-provider binary
internal/
  broker/       → Broker server, endpoint registry, relay logic
  consumer/     → SOCKS5 server, yamux client, tunnel dialer
  provider/     → Provider client, target dialer, header scrubber
  transport/    → HTTP long-poll transport, pipe-based session, httpconn adapter
  config/       → YAML config loading, logger setup
configs/        → Example YAML configuration files
plans/          → Architecture documentation
```

For a detailed technical design, see [plans/architecture.md](plans/architecture.md).

## Security Notes

- **HTTP vs HTTPS**: By default, traffic between nodes uses plain HTTP. For production use, enable TLS on the Broker (`tls.enabled: true` in `broker.yaml`) or place it behind a reverse proxy with TLS termination. Without TLS, tunnel traffic is visible to network observers.

- **Header Scrubbing**: The Provider can strip headers like `X-Forwarded-For`, `Via`, and `Proxy-Authorization` that reveal proxy usage. Enable with `scrub_headers: true` in the Provider config or `--scrub-headers` on the CLI.

- **Authentication**: The auth middleware is a placeholder. There is currently no authentication between nodes. Do not expose the Broker to the public internet without adding authentication or restricting access by IP.

- **DNS Privacy**: DNS queries are resolved on the Provider (Machine C). This means your local DNS resolver never sees the domains you visit through the tunnel, but the Provider's DNS resolver does.

- **Endpoint Naming**: Anyone who knows the Broker URL and endpoint name can connect as a Consumer or Provider. Treat endpoint names as shared secrets until proper authentication is implemented.

## Raspberry Pi Deployment

The Broker is designed to run on a Raspberry Pi as a lightweight relay:

```bash
# Build for Raspberry Pi 3/4/5 (64-bit OS)
make build-pi

# Build for older Raspberry Pi (32-bit OS)
make build-pi-armv7

# Copy to Pi
scp bin/httpbroker-broker-arm64 pi@raspberrypi:~/httpbroker-broker
scp configs/broker.yaml pi@raspberrypi:~/broker.yaml

# Run on Pi
ssh pi@raspberrypi
./httpbroker-broker --config broker.yaml --listen :8080
```

## License

See [LICENSE](LICENSE) for details.
