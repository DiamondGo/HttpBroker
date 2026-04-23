# Status Endpoint Security Enhancement

## Summary

Added a configuration option to disable the `/status` endpoint by default for improved security.

## Changes Made

### 1. Configuration Structure

**File: `internal/config/config.go`**
- Added `StatusEndpointEnabled` field to `ServerConfig` struct
- Default value: `false` (disabled for security)

**File: `internal/broker/server.go`**
- Added `StatusEndpointEnabled` field to `Config` struct
- Modified router setup to conditionally register `/status` endpoint
- Added logging to indicate whether status endpoint is enabled or disabled

### 2. Command-Line Interface

**File: `cmd/broker/main.go`**
- Added `--enable-status` CLI flag to enable the status endpoint
- CLI flag overrides config file setting

### 3. Configuration Files

**Updated configuration files:**
- `configs/broker.yaml`: Added `status_endpoint_enabled: false` (default)
- `configs/broker-with-auth-example.yaml`: Added `status_endpoint_enabled: false`
- `configs/broker-debug.yaml`: Added `status_endpoint_enabled: true` (for debugging)

### 4. Tests

**File: `internal/broker/server_test.go`** (new file)
- `TestStatusEndpointEnabled`: Verifies `/status` returns 200 when enabled
- `TestStatusEndpointDisabled`: Verifies `/status` returns 404 when disabled

**File: `integration_test.go`**
- Updated `launchBroker()` to use `--enable-status` flag for tests

### 5. Documentation

**File: `AUTHENTICATION.md`**
- Added new section: "Status Endpoint Security"
- Documented how to enable the endpoint
- Listed security recommendations

**File: `README.md`**
- Updated health check section
- Added note about default disabled state
- Added reference to security documentation

## Usage Examples

### Enable via Configuration File

```yaml
# configs/broker.yaml
server:
  listen: ":8080"
  status_endpoint_enabled: true  # Enable status endpoint
```

### Enable via Command-Line Flag

```bash
./bin/httpbroker-broker --enable-status
```

### Check Status (when enabled)

```bash
curl http://localhost:8080/status
```

**Response:**
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

### Disabled Behavior (default)

```bash
curl http://localhost:8080/status
```

**Response:**
```
404 page not found
```

## Security Rationale

The `/status` endpoint was previously always enabled and exposed information about:
- Active endpoint names
- Provider connection status
- Number of connected consumers

While not critically sensitive, this information could be useful to attackers for:
- Network reconnaissance
- Identifying active tunnels
- Planning denial-of-service attacks

By disabling it by default:
- Reduces attack surface
- Prevents information leakage
- Follows principle of least privilege
- Can still be enabled when needed for monitoring

## Migration Guide

Existing deployments will continue to work, but the `/status` endpoint will be disabled by default.

**To restore previous behavior (enable status endpoint):**

1. Add to config file:
   ```yaml
   server:
     status_endpoint_enabled: true
   ```

2. Or use CLI flag:
   ```bash
   ./bin/httpbroker-broker --enable-status
   ```

## Testing

All tests pass:
- `go test ./internal/broker/` ✓
- `cd test && go test .` ✓
- Integration tests will automatically enable status endpoint for health checks

## Breaking Changes

None. The change is backward compatible:
- Existing configurations will work (status endpoint disabled)
- CLI flags can enable it without config file changes
- Tests updated to explicitly enable when needed
