# Status Endpoint Feature Demo

## Quick Overview

The `/status` endpoint is now **disabled by default** for security. This demo shows how to enable and use it.

## Demo 1: Default Behavior (Disabled)

Start broker with default config:
```bash
./bin/httpbroker-broker --config configs/broker.yaml
```

Console output:
```
INFO  broker/server.go:94  status endpoint disabled
```

Try to access `/status`:
```bash
curl http://localhost:8080/status
```

**Result:** `404 page not found`

---

## Demo 2: Enable via CLI Flag

Start broker with status endpoint enabled:
```bash
./bin/httpbroker-broker --config configs/broker.yaml --enable-status
```

Console output:
```
INFO  broker/server.go:92  status endpoint enabled at GET /status
```

Access `/status`:
```bash
curl http://localhost:8080/status | jq
```

**Result:**
```json
{
  "version": "",
  "endpoints": []
}
```

After starting provider and consumer:
```json
{
  "version": "",
  "endpoints": [
    {
      "name": "default",
      "has_provider": true,
      "consumer_count": 1
    }
  ]
}
```

---

## Demo 3: Enable via Config File

Edit `configs/broker.yaml`:
```yaml
server:
  listen: ":8080"
  status_endpoint_enabled: true  # ← Add this line
```

Start broker:
```bash
./bin/httpbroker-broker --config configs/broker.yaml
```

Console output:
```
INFO  broker/server.go:92  status endpoint enabled at GET /status
```

---

## Demo 4: Debug Mode (Pre-configured)

The debug config has it enabled by default:
```bash
./bin/httpbroker-broker --config configs/broker-debug.yaml
```

Console output:
```
INFO  broker/server.go:92  status endpoint enabled at GET /status
```

---

## Security Comparison

### Before This Change
- `/status` always accessible ❌
- Information leakage risk ⚠️
- No way to disable it

### After This Change
- `/status` disabled by default ✅
- Explicit opt-in required 🔒
- Flexible configuration options

---

## Use Cases

### When to Enable
- Development/debugging
- Monitoring systems (Prometheus, etc.)
- Health checks behind firewall
- Internal infrastructure

### When to Keep Disabled
- Public-facing brokers
- Untrusted networks
- Production without monitoring
- Security-critical deployments

---

## Command-Line Options

```bash
# View help
./bin/httpbroker-broker --help

# Enable status endpoint
./bin/httpbroker-broker --enable-status

# Combine with other options
./bin/httpbroker-broker --listen :9090 --enable-status

# Override config file setting
./bin/httpbroker-broker --config configs/broker.yaml --enable-status
```

---

## Testing

Run the test suite:
```bash
# Unit tests
go test ./internal/broker/ -v -run TestStatusEndpoint

# Integration tests (automatically enable status for health checks)
go test -v -timeout 300s -run TestIntegration ./...
```

---

## Configuration Reference

### YAML Config
```yaml
server:
  listen: ":8080"
  tls:
    enabled: false
    cert_file: ""
    key_file: ""
  status_endpoint_enabled: false  # true to enable
```

### CLI Flags
```
--enable-status     enable GET /status endpoint
--listen string     override listen address
--config string     path to config file
```

### Precedence
CLI flag `--enable-status` > Config file `status_endpoint_enabled`

---

## Monitoring Integration Example

### With Prometheus

Enable status endpoint in production behind firewall:
```yaml
server:
  listen: ":8080"
  status_endpoint_enabled: true
```

Scrape config:
```yaml
scrape_configs:
  - job_name: 'httpbroker'
    static_configs:
      - targets: ['localhost:8080']
    metrics_path: '/status'
```

### With Health Check Scripts

```bash
#!/bin/bash
STATUS=$(curl -s http://localhost:8080/status 2>/dev/null)
if [ $? -eq 0 ]; then
    echo "Broker is healthy"
    echo "$STATUS" | jq .
else
    echo "Broker is down or status endpoint disabled"
    exit 1
fi
```
