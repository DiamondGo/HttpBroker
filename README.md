# HttpBroker

**A lightweight HTTP-based NAT traversal and intranet penetration solution** that lets machines behind firewalls, NAT, or restrictive networks access resources through an intermediary provider. Works as a reverse proxy tunnel using only standard HTTP/HTTPS traffic — no VPN client required, no firewall modifications needed.

## Key Features

- 🌐 **NAT Traversal / Intranet Penetration** — Access services behind NAT, firewalls, or private networks without port forwarding or VPN configuration
- 🔒 **Firewall-Friendly** — Uses standard HTTP/HTTPS traffic that looks like ordinary web API calls — bypasses restrictive corporate proxies and firewalls
- 🚀 **Zero Infrastructure** — No VPN server setup, no complex routing, no iptables rules — just three simple binaries
- 🎯 **SOCKS5 Proxy** — Works with any browser or application that supports SOCKS5 (Chrome, Firefox, curl, SSH, etc.)
- 🔀 **Reverse Proxy Tunnel** — Provider machines behind NAT can expose their network without incoming port forwarding
- 📡 **HTTP Long-Polling Transport** — Maintains persistent tunnels using HTTP long-polling — compatible with most corporate proxy servers
- 🔐 **Privacy-Preserving** — DNS queries are resolved on the provider side, hiding your browsing destinations from local DNS
- 🔑 **SSH Tunnel Support** — Tunnel SSH connections through the proxy to access remote servers behind NAT/firewalls

## Use Cases

- **Remote Access to Private Networks** — Access internal services (databases, APIs, admin panels) running behind NAT or corporate firewalls
- **SSH to Internal Servers** — SSH into machines behind NAT/firewalls without port forwarding or VPN setup
- **Bypass Network Restrictions** — Route traffic through a provider in a less restrictive network location
- **Development & Testing** — Test webhooks and external integrations against services running on your local machine
- **IoT & Home Networks** — Access devices on home networks without exposing them directly to the internet
- **Alternative to VPN** — Simpler setup than traditional VPN for proxy-based traffic routing

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

## NAT Traversal & Intranet Penetration

HttpBroker excels at **NAT traversal** and **intranet penetration** scenarios where traditional VPN or direct connections are impractical:

### How NAT Traversal Works

The key insight is that **the Provider initiates all connections** to the Broker. This means:

- ✅ **No incoming port forwarding required** on the Provider's network
- ✅ **Works behind carrier-grade NAT (CGNAT)** where public IP addresses are shared
- ✅ **Bypasses restrictive firewalls** that only allow outbound HTTP/HTTPS traffic
- ✅ **Traverses corporate proxies** since all traffic looks like standard web API calls

### Comparison with Traditional Solutions

| Feature | HttpBroker | Traditional VPN | Port Forwarding | Other Tunnels |
|---------|------------|-----------------|-----------------|---------------|
| **NAT Traversal** | ✅ Built-in | ⚠️ Requires configuration | ❌ Needs public IP | ✅ Varies |
| **Firewall-Friendly** | ✅ HTTP/HTTPS only | ❌ Special protocols | ❌ Incoming traffic | ⚠️ May be blocked |
| **Setup Complexity** | ✅ Low (3 binaries) | ❌ High (server setup, certs, routing) | ⚠️ Medium (router config) | ⚠️ Varies |
| **Corporate Proxy Compatible** | ✅ Yes | ❌ Usually blocked | ❌ N/A | ❌ Usually blocked |
| **Works Behind CGNAT** | ✅ Yes | ⚠️ Difficult | ❌ No | ✅ Usually yes |
| **Privacy from Local Network** | ✅ DNS on provider | ✅ Full encryption | ❌ Local DNS | ⚠️ Varies |

### Real-World Scenarios

**Scenario 1: Home Network Behind CGNAT**
```
Problem: ISP uses CGNAT, you don't have a public IP address
Solution: Deploy Provider on home network, Broker on VPS, Consumer on laptop
Result: Access home services (NAS, IoT devices) from anywhere
```

**Scenario 2: Restrictive Corporate Network**
```
Problem: Corporate firewall blocks VPN protocols, only allows HTTP/HTTPS
Solution: Deploy Provider on personal server, Broker on cloud, Consumer on work laptop
Result: Bypass restrictions using traffic that looks like web browsing
```

**Scenario 3: Development Testing**
```
Problem: Need to test webhooks against a service running on localhost
Solution: Deploy Provider on dev machine, Broker on VPS, Consumer on test machine
Result: External services can reach your local server via the tunnel
```

**Scenario 4: IoT Device Management**
```
Problem: Manage IoT devices on customer sites behind various NAT configurations
Solution: Deploy Provider on-site, Broker on cloud, Consumer on admin workstation
Result: Consistent management interface regardless of customer network topology
```

**Scenario 5: SSH Access to Home Server**
```
Problem: Need to SSH into your home server while traveling, but ISP uses CGNAT
Solution: Deploy Provider on home server, Broker on cheap VPS, Consumer on laptop
Result: ssh -o ProxyCommand='nc -x 127.0.0.1:1080 %h %p' user@homeserver.local
        Access your home server from anywhere without exposing SSH port to internet
```

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

**Health check (requires --enable-status flag):**

```bash
# Enable status endpoint for monitoring
./bin/httpbroker-broker --listen :8080 --enable-status

# Check broker health
curl http://BROKER_IP:8080/status
```

**Note:** The `/status` endpoint is **disabled by default** for security. Enable it with `--enable-status` flag or set `status_endpoint_enabled: true` in the config file. See [AUTHENTICATION.md](AUTHENTICATION.md#status-endpoint-security) for security considerations.

#### Generating Self-Signed Certificates for HTTPS

To enable TLS/HTTPS on the Broker, you need a certificate and private key. For testing or internal use, you can generate a self-signed certificate using OpenSSL:

```bash
# Generate a self-signed certificate valid for 365 days
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -days 365 -nodes \
  -subj "/C=US/ST=State/L=City/O=Organization/CN=your-broker-hostname"

# If you need to specify IP address or multiple hostnames, use SAN (Subject Alternative Name)
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -days 365 -nodes \
  -subj "/C=US/ST=State/L=City/O=Organization/CN=your-broker-hostname" \
  -addext "subjectAltName=DNS:your-broker-hostname,DNS:localhost,IP:192.168.1.100"
```

Replace `your-broker-hostname` with your broker's hostname or domain name, and `192.168.1.100` with your broker's IP address.

**Start the Broker with TLS:**

```bash
# Using CLI flags
./bin/httpbroker-broker --listen :8443 --tls-cert server.crt --tls-key server.key

# Or update configs/broker.yaml:
# server:
#   listen: ":8443"
#   tls:
#     enabled: true
#     cert_file: "server.crt"
#     key_file: "server.key"
./bin/httpbroker-broker --config configs/broker.yaml
```

**Update Consumer and Provider to use HTTPS:**

When using self-signed certificates, you have two options:

**Option 1: Skip certificate verification (for testing only):**

```bash
# Consumer - using CLI flag
./bin/httpbroker-consumer --broker-url https://BROKER_IP:8443 --endpoint vpn1 --socks5-listen :1080 --insecure-skip-verify

# Provider - using CLI flag
./bin/httpbroker-provider --broker-url https://BROKER_IP:8443 --endpoint vpn1 --insecure-skip-verify

# Or update configs/consumer.yaml and configs/provider.yaml:
# broker:
#   url: "https://BROKER_IP:8443"
#   endpoint: "vpn1"
#   insecure_skip_verify: true  # Skip TLS cert verification
```

With `insecure_skip_verify: true`, the certificate's CN (Common Name) and other fields don't need to match the actual hostname or IP address.

**Option 2: Use properly configured certificates (for production):**

Generate certificates with matching CN/SAN and use them without skipping verification:

```bash
# Consumer
./bin/httpbroker-consumer --broker-url https://BROKER_IP:8443 --endpoint vpn1 --socks5-listen :1080

# Provider
./bin/httpbroker-provider --broker-url https://BROKER_IP:8443 --endpoint vpn1
```

**Security Note:** `insecure_skip_verify` disables all TLS certificate verification and should **only be used for testing**. For production use, either:
- Use certificates from a trusted Certificate Authority (CA) like Let's Encrypt
- Generate certificates with properly configured CN/SAN fields that match your broker's hostname/IP
- Import your self-signed CA certificate into the system's trust store

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

### SSH (Remote Server Access)

You can tunnel SSH connections through the SOCKS5 proxy to access remote servers behind NAT or firewalls:

**Method 1: Using ProxyCommand with netcat**
```bash
# One-time connection
ssh -o ProxyCommand='nc -x 127.0.0.1:1080 %h %p' user@remote-server.internal

# HTTPS connection
ssh -o ProxyCommand='nc -X connect -x 127.0.0.1:1080 %h %p' user@remote-server.internal
```

**Method 2: Configure in ~/.ssh/config**
```bash
# Edit ~/.ssh/config
Host remote-server.internal
    ProxyCommand nc -x 127.0.0.1:1080 %h %p
    User your-username

# Or for all hosts through this proxy
Host *.internal
    ProxyCommand nc -x 127.0.0.1:1080 %h %p

# Then simply connect
ssh remote-server.internal
```

**Method 3: Using ssh native SOCKS support (OpenSSH 7.6+)**
```bash
# One-time connection
ssh -o ProxyCommand='ssh -W %h:%p -o ProxyCommand="nc -x 127.0.0.1:1080 localhost 22" jumphost' user@remote-server

# Or simpler with direct SOCKS proxy
ssh -o ProxyCommand='socat - SOCKS4A:127.0.0.1:%h:%p,socksport=1080' user@remote-server.internal
```

**Method 4: Using ProxyJump (for multi-hop)**
```bash
# First configure the proxy in ~/.ssh/config
Host proxy-tunnel
    HostName 127.0.0.1
    Port 1080
    ProxyCommand nc -x 127.0.0.1:1080 %h %p

Host internal-server
    HostName remote-server.internal
    User your-username
    ProxyJump proxy-tunnel

# Then connect
ssh internal-server
```

**Use Cases for SSH Tunneling:**
- Access servers behind NAT without port forwarding
- SSH to machines on corporate networks from outside
- Manage IoT devices or embedded systems behind firewalls
- Access home servers when traveling
- Connect to development machines in restricted networks
- Bypass SSH restrictions in corporate environments

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
  status_endpoint_enabled: false  # Expose GET /status endpoint (default: false)
  unauthorized_redirect:
    enabled: false         # Redirect unauthorized requests instead of returning 401/404
    url: ""                # Redirect target URL (see below for format options)

tunnel:
  poll_timeout: "5s"       # How long to hold a poll request before returning empty
  session_timeout: "5m"    # Disconnect sessions idle longer than this

auth:
  enabled: false           # Enable Bearer token authentication
  token: ""                # Shared secret token (required when enabled)

logging:
  level: "info"            # Log level: debug, info, warn, error
```

**Unauthorized Redirect URL Formats:**

The `unauthorized_redirect.url` field supports three formats:

1. **Same-site path**: `/login` — Redirects to a path on the same server
2. **Domain only**: `www.example.com` — Auto-prefixed with `http://` or `https://` based on the current request scheme
3. **Full URL**: `https://www.example.com` — Used as-is, regardless of current request scheme

When enabled, unauthorized requests (invalid/missing auth tokens or non-tunnel paths) return `302 Found` instead of `401 Unauthorized` or `404 Not Found`. This prevents revealing that the server is a broker to unauthorized parties.

### Consumer (`configs/consumer.yaml`)

```yaml
broker:
  url: "http://127.0.0.1:8080"     # Broker URL
  endpoint: "default"               # Endpoint name (must match Provider)
  insecure_skip_verify: false       # Skip TLS cert verification (use only for testing)
  auth_token: ""                    # Authentication token (must match Broker's token)

socks5:
  listen: ":1080"                   # Local SOCKS5 listen address

transport:
  poll_interval: "50ms"             # Delay between poll requests
  retry_backoff: "5s"               # Wait time before reconnecting on error

logging:
  level: "info"                     # Log level: debug, info, warn, error
```

### Provider (`configs/provider.yaml`)

```yaml
broker:
  url: "http://127.0.0.1:8080"     # Broker URL
  endpoint: "default"               # Endpoint name (must match Consumer)
  insecure_skip_verify: false       # Skip TLS cert verification (use only for testing)
  auth_token: ""                    # Authentication token (must match Broker's token)

provider:
  scrub_headers: true               # Strip proxy-revealing HTTP headers
  dial_timeout: "10s"               # Timeout when dialing target hosts

transport:
  poll_interval: "50ms"             # Delay between poll requests
  retry_backoff: "5s"               # Wait time before reconnecting on error

logging:
  level: "info"                     # Log level: debug, info, warn, error
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

- **Authentication**: HttpBroker supports Bearer token authentication. When enabled, all Consumer and Provider connections must include a valid authentication token:

  ```yaml
  # Broker config
  auth:
    enabled: true
    token: "your-secret-token-here"

  # Consumer/Provider config
  broker:
    auth_token: "your-secret-token-here"  # Must match Broker's token
  ```

  **Important**: Bearer tokens are sent in HTTP headers. If you enable authentication, you **must** also enable TLS (`tls.enabled: true`) to encrypt the token in transit. Using authentication over plain HTTP exposes your token to network observers.

- **Header Scrubbing**: The Provider can strip headers like `X-Forwarded-For`, `Via`, and `Proxy-Authorization` that reveal proxy usage. Enable with `scrub_headers: true` in the Provider config or `--scrub-headers` on the CLI.

- **DNS Privacy**: DNS queries are resolved on the Provider (Machine C). This means your local DNS resolver never sees the domains you visit through the tunnel, but the Provider's DNS resolver does.

- **Endpoint Naming**: Endpoint names help organize multiple tunnels. When authentication is disabled, anyone who knows the Broker URL and endpoint name can connect. Enable authentication to secure your Broker.

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
