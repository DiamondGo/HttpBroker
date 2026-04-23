# HttpBroker Speed Test Script

## Overview

The `speedtest.sh` script performs comprehensive bandwidth and latency testing for the HttpBroker SOCKS5 proxy. It tests download speed, upload speed, and HTTPS latency, then generates a detailed report.

## Requirements

- **curl**: For making HTTP/HTTPS requests through the SOCKS5 proxy
- **bc**: For floating-point calculations
- **nc** (netcat): For checking proxy connectivity

## Usage

### Quick Start

1. **Start the HttpBroker services**:
   ```bash
   # Terminal 1: Start broker
   ./bin/httpbroker-broker --config configs/broker-debug.yaml
   
   # Terminal 2: Start provider
   ./bin/httpbroker-provider --config configs/provider-debug.yaml
   
   # Terminal 3: Start consumer
   ./bin/httpbroker-consumer --config configs/consumer-debug.yaml
   ```

2. **Run the speed test**:
   ```bash
   # Using default SOCKS5 address (127.0.0.1:10800)
   make speedtest
   
   # Or run directly
   ./scripts/speedtest.sh
   
   # Using custom SOCKS5 address
   ./scripts/speedtest.sh 127.0.0.1:1080
   ```

## Test Breakdown

The script performs the following tests:

### 1. Connectivity Check
- Verifies the SOCKS5 proxy is accessible
- Tests basic HTTP request latency

### 2. Download Speed Test
Tests download speeds with multiple file sizes:
- **1 MB** - Quick test for small files
- **5 MB** - Medium file size
- **10 MB** - Larger file to measure sustained throughput

Uses Cloudflare's speed test endpoint for reliable testing.

### 3. Upload Speed Test
Tests upload speeds with:
- **1 MB** - Small upload
- **5 MB** - Medium upload

Uses httpbin.org as the upload target.

### 4. HTTPS Latency Test
Performs 5 consecutive HTTPS requests to measure:
- Connection establishment time
- TLS handshake overhead
- Request/response latency

### 5. Report Generation
Generates a comprehensive report with:
- Performance metrics (download/upload speeds, latency)
- Performance ratings (Excellent/Good/Fair)
- Usage recommendations based on test results
- Saved report file in `/tmp/httpbroker-speedtest/`

## Understanding the Results

### Download Speed
- **Excellent**: ≥50 Mbps - Great for all use cases
- **Good**: ≥20 Mbps - Suitable for web browsing and downloads
- **Fair**: <20 Mbps - Basic browsing only

### Upload Speed
- **Excellent**: ≥10 Mbps - Good for file uploads
- **Good**: ≥1 Mbps - Suitable for light uploads
- **Fair**: <1 Mbps - Very limited upload capability

### Latency
- **Excellent**: <1s - Low latency, good for interactive applications
- **Moderate**: 1-3s - Acceptable for most use cases
- **High**: >3s - May affect user experience

## Sample Output

```
╔════════════════════════════════════════════════════════╗
║        HttpBroker SOCKS5 Proxy Speed Test             ║
╠════════════════════════════════════════════════════════╣
║  Proxy Address: 127.0.0.1:10800                        ║
╚════════════════════════════════════════════════════════╝

[1/5] Testing SOCKS5 proxy connectivity...
✓ SOCKS5 proxy is accessible

[2/5] Testing basic HTTP request...
✓ HTTP request successful
  Latency: 0.156 seconds

[3/5] Testing download speed...
  Testing with different file sizes:
  - Downloading 1 MB... ✓ Time: 0.337s, Speed: 26.20 Mbps
  - Downloading 5 MB... ✓ Time: 0.724s, Speed: 53.00 Mbps
  - Downloading 10 MB... ✓ Time: 1.253s, Speed: 60.60 Mbps
  Average Download Speed: 43.30 Mbps

[4/5] Testing upload speed...
  Testing with different file sizes:
  - Uploading 1 MB... ✓ Time: 7.850s, Speed: 1.00 Mbps
  - Uploading 5 MB... ✓ Time: 25.110s, Speed: 1.60 Mbps
  Average Upload Speed: 1.30 Mbps

[5/5] Testing HTTPS latency...
  Testing 5 consecutive HTTPS requests:
  - Request 1... ✓ Time: 0.342s
  - Request 2... ✓ Time: 0.354s
  - Request 3... ✓ Time: 0.353s
  - Request 4... ✓ Time: 0.361s
  - Request 5... ✓ Time: 0.348s
  Average HTTPS Latency: 0.352s

╔════════════════════════════════════════════════════════╗
║                    Final Report                        ║
╠════════════════════════════════════════════════════════╣
║ Metric                              Value              ║
╠════════════════════════════════════════════════════════╣
║ Basic HTTP Latency                  0.156s             ║
║ Average HTTPS Latency               0.352s             ║
║ Average Download Speed              43.30 Mbps         ║
║ Average Upload Speed                1.30 Mbps          ║
╠════════════════════════════════════════════════════════╣
║ Download Performance                Good               ║
║ Upload Performance                  Good               ║
╠════════════════════════════════════════════════════════╣
║                 Usage Recommendations                  ║
╠════════════════════════════════════════════════════════╣
║ ✓ Suitable for web browsing and API calls             ║
║ ✓ Suitable for file downloads                         ║
║ ✓ Suitable for light data uploads                     ║
║ ! Upload speed limited - avoid large file uploads     ║
║ ✓ Low latency - excellent for interactive apps        ║
╚════════════════════════════════════════════════════════╝

✓ Report saved to: /tmp/httpbroker-speedtest/speedtest-report-20260330-153704.txt
```

## Troubleshooting

### Error: SOCKS5 proxy is not accessible
**Solution**: Make sure all three services (broker, provider, consumer) are running:
```bash
# Check if consumer is running on the expected port
lsof -i :10800

# Restart consumer if needed
./bin/httpbroker-consumer --config configs/consumer-debug.yaml
```

### Error: HTTP request failed
**Possible causes**:
- Network connectivity issues
- Provider not properly connected to broker
- Firewall blocking outbound connections

**Solution**: Check logs of all three services for errors.

### Slow speeds or timeouts
**Possible causes**:
- Network congestion
- Long `poll_timeout` setting (default: 45s in broker config)
- Provider's network connection is slow
- High latency between broker and provider/consumer

**Solution**:
- Check provider's actual network speed
- Verify all three services (broker, provider, consumer) are running and connected
- Check broker logs for session activity and errors
- Consider reducing `poll_timeout` in broker config for faster responses (trade-off: more frequent polling)

## Advanced Usage

### Custom Test Parameters

You can modify the script to test with different file sizes or URLs:

```bash
# Edit the script
vi scripts/speedtest.sh

# Change these arrays:
declare -a SIZES=("1000000" "5000000" "10000000" "50000000")  # Add 50MB test
declare -a LABELS=("1 MB" "5 MB" "10 MB" "50 MB")
```

### Running Multiple Tests

For more accurate results, run the test multiple times:
```bash
for i in {1..3}; do
  echo "=== Test Run $i ==="
  ./scripts/speedtest.sh
  sleep 5
done
```

## Report Files

All test reports are saved to `/tmp/httpbroker-speedtest/` with timestamps:
- **Format**: `speedtest-report-YYYYMMDD-HHMMSS.txt`
- **Content**: Plain text file with all test results and metrics
- **Cleanup**: Temporary test files are automatically cleaned up after each run

