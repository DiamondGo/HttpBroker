# Authentication Guide

HttpBroker supports Bearer Token authentication to secure connections between the Broker, Consumers, and Providers.

## Overview

When authentication is enabled:
- The Broker validates all incoming requests for a valid Bearer token
- Consumers and Providers must include the token in every request
- Invalid or missing tokens result in `401 Unauthorized` responses

## Quick Start

### 1. Generate a Strong Token

Use one of these methods to generate a secure random token:

```bash
# Using OpenSSL (recommended)
openssl rand -base64 32

# Using Python
python3 -c "import secrets; print(secrets.token_urlsafe(32))"

# Using /dev/urandom
head -c 32 /dev/urandom | base64
```

Example output: `dGhpc19pc19hX3NlY3JldF90b2tlbl9leGFtcGxl`

### 2. Configure the Broker

Edit `configs/broker.yaml`:

```yaml
auth:
  enabled: true
  token: "dGhpc19pc19hX3NlY3JldF90b2tlbl9leGFtcGxl"
```

### 3. Configure Consumers and Providers

Edit `configs/consumer.yaml` and `configs/provider.yaml`:

```yaml
broker:
  url: "http://127.0.0.1:8080"
  endpoint: "default"
  auth_token: "dGhpc19pc19hX3NlY3JldF90b2tlbl9leGFtcGxl"  # Must match broker's token
```

### 4. Start the Services

```bash
# Start broker
./bin/httpbroker-broker --config configs/broker.yaml

# Start provider
./bin/httpbroker-provider --config configs/provider.yaml

# Start consumer
./bin/httpbroker-consumer --config configs/consumer.yaml
```

## Security Best Practices

### ⚠️ CRITICAL: Always Use TLS with Authentication

**Never use authentication over plain HTTP in production!**

Bearer tokens are sent in HTTP headers. Without TLS, they are transmitted in clear text and can be intercepted by network observers.

**Correct configuration:**

```yaml
# Broker
server:
  tls:
    enabled: true
    cert_file: "/path/to/cert.pem"
    key_file: "/path/to/key.pem"

auth:
  enabled: true
  token: "your-secret-token"
```

### Token Management

- **Keep tokens secret**: Treat tokens like passwords
- **Use strong tokens**: At least 32 random bytes (base64-encoded)
- **Rotate regularly**: Change tokens periodically (requires updating all configs)
- **Use different tokens per environment**: Don't reuse production tokens in testing

### Self-Signed Certificates

For internal use, you can use self-signed certificates:

```bash
# Generate self-signed certificate
openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -days 365 -nodes \
  -subj "/CN=your-broker-hostname"
```

Configure clients to skip certificate verification (only for testing/internal use):

```yaml
broker:
  insecure_skip_verify: true  # Only use in trusted networks!
```

## Testing Authentication

### Test with curl

```bash
# Without token (should fail with 401)
curl -X POST http://localhost:8080/tunnel/connect?role=consumer&endpoint=test

# With valid token (should succeed)
curl -X POST http://localhost:8080/tunnel/connect?role=consumer&endpoint=test \
  -H "Authorization: Bearer dGhpc19pc19hX3NlY3JldF90b2tlbl9leGFtcGxl"
```

### Expected Error Response

When authentication fails:

```json
{
  "error": "unauthorized"
}
```

HTTP status: `401 Unauthorized`

## Troubleshooting

### Consumer/Provider Cannot Connect

**Symptom**: Client logs show "broker returned status 401"

**Solutions**:
1. Verify `auth_token` in client config matches broker's `auth.token`
2. Check for extra spaces or newlines in token strings
3. Ensure broker has `auth.enabled: true`

### Token in Logs

**Issue**: Never log tokens in plain text

The broker and clients do not log token values. If you need to debug:
- Verify token length (base64-encoded 32 bytes ≈ 44 characters)
- Compare token file checksums: `echo -n "token" | sha256sum`

## Disabling Authentication

To disable authentication (for testing only):

```yaml
# Broker
auth:
  enabled: false
```

## Status Endpoint Security

The `/status` endpoint is **disabled by default** for security reasons. It exposes information about active endpoints, provider connections, and consumer counts.

### Enabling the Status Endpoint

**In configuration file** (`configs/broker.yaml`):

```yaml
server:
  status_endpoint_enabled: true  # Enable GET /status endpoint
```

**Via command-line flag**:

```bash
./bin/httpbroker-broker --enable-status
```

### What Information is Exposed?

When enabled, `GET /status` returns:

```json
{
  "version": "v1.0.0",
  "endpoints": [
    {
      "name": "default",
      "has_provider": true,
      "consumer_count": 2
    }
  ]
}
```

### Security Recommendations

- **Keep disabled in production** unless you have a specific monitoring need
- If enabled, consider:
  - Using a reverse proxy with IP whitelisting
  - Adding custom authentication middleware
  - Placing it behind a VPN or internal network
- The endpoint does NOT require authentication by design (for health checks)
- Monitor access logs for unexpected `/status` requests

Remove or leave empty the `auth_token` field in consumer/provider configs:

```yaml
broker:
  auth_token: ""  # Empty = no authentication
```

## Example Configurations

See the example configuration files:
- `configs/broker-with-auth-example.yaml`
- `configs/consumer-with-auth-example.yaml`
- `configs/provider-with-auth-example.yaml`

