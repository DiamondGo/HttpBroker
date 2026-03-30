#!/bin/bash

# HttpBroker Speed Test Script
# Tests download and upload speeds through the SOCKS5 proxy
# Usage: ./scripts/speedtest.sh [socks5_address]

set -e

# Configuration
SOCKS5_ADDR="${1:-127.0.0.1:10800}"
DOWNLOAD_URL="https://speed.cloudflare.com/__down"
UPLOAD_URL="https://httpbin.org/post"
TMP_DIR="/tmp/httpbroker-speedtest"

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Create temp directory
mkdir -p "$TMP_DIR"

# Print header
echo -e "${BLUE}╔════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║        HttpBroker SOCKS5 Proxy Speed Test             ║${NC}"
echo -e "${BLUE}╠════════════════════════════════════════════════════════╣${NC}"
echo -e "${BLUE}║  Proxy Address: ${GREEN}${SOCKS5_ADDR}${BLUE}                        ║${NC}"
echo -e "${BLUE}╚════════════════════════════════════════════════════════╝${NC}"
echo ""

# Test connectivity
echo -e "${YELLOW}[1/5] Testing SOCKS5 proxy connectivity...${NC}"
if ! nc -z $(echo $SOCKS5_ADDR | tr ':' ' ') 2>/dev/null; then
    echo -e "${RED}✗ SOCKS5 proxy is not accessible at ${SOCKS5_ADDR}${NC}"
    echo -e "${YELLOW}  Please start the consumer with: ./bin/httpbroker-consumer --config configs/consumer.yaml${NC}"
    exit 1
fi
echo -e "${GREEN}✓ SOCKS5 proxy is accessible${NC}"
echo ""

# Test basic HTTP request
echo -e "${YELLOW}[2/5] Testing basic HTTP request...${NC}"
START_TIME=$(date +%s.%N)
if curl --max-time 15 --socks5-hostname "$SOCKS5_ADDR" -s http://example.com/ > /dev/null; then
    END_TIME=$(date +%s.%N)
    LATENCY=$(echo "$END_TIME - $START_TIME" | bc)
    echo -e "${GREEN}✓ HTTP request successful${NC}"
    printf "${BLUE}  Latency: %.3f seconds${NC}\n" "$LATENCY"
else
    echo -e "${RED}✗ HTTP request failed${NC}"
    exit 1
fi
echo ""

# Download speed test
echo -e "${YELLOW}[3/5] Testing download speed...${NC}"
echo -e "${BLUE}  Testing with different file sizes:${NC}"

declare -a SIZES=("1000000" "5000000" "10000000")
declare -a LABELS=("1 MB" "5 MB" "10 MB")
TOTAL_DOWNLOAD_TIME=0
TOTAL_DOWNLOAD_SIZE=0

for i in "${!SIZES[@]}"; do
    SIZE="${SIZES[$i]}"
    LABEL="${LABELS[$i]}"
    
    printf "${BLUE}  - Downloading ${LABEL}...${NC} "
    
    START_TIME=$(date +%s.%N)
    if curl --max-time 60 --socks5-hostname "$SOCKS5_ADDR" -s \
    "${DOWNLOAD_URL}?bytes=${SIZE}" -o "$TMP_DIR/download-${SIZE}.bin" 2>/dev/null; then
        END_TIME=$(date +%s.%N)
        ELAPSED=$(echo "$END_TIME - $START_TIME" | bc)
        
        # Get actual file size
        ACTUAL_SIZE=$(stat -f%z "$TMP_DIR/download-${SIZE}.bin" 2>/dev/null || stat -c%s "$TMP_DIR/download-${SIZE}.bin" 2>/dev/null)
        
        # Calculate speed in Mbps
        BYTES_PER_SEC=$(echo "scale=2; $ACTUAL_SIZE / $ELAPSED" | bc)
        MBPS=$(echo "scale=2; $BYTES_PER_SEC * 8 / 1000000" | bc)
        
        printf "${GREEN}✓${NC} "
        printf "Time: %.3fs, Speed: ${GREEN}%.2f Mbps${NC}\n" "$ELAPSED" "$MBPS"
        
        TOTAL_DOWNLOAD_TIME=$(echo "$TOTAL_DOWNLOAD_TIME + $ELAPSED" | bc)
        TOTAL_DOWNLOAD_SIZE=$(echo "$TOTAL_DOWNLOAD_SIZE + $ACTUAL_SIZE" | bc)
    else
        echo -e "${RED}✗ Failed${NC}"
    fi
done

# Calculate average download speed
if (( $(echo "$TOTAL_DOWNLOAD_TIME > 0" | bc -l) )); then
    AVG_DOWNLOAD_BYTES_PER_SEC=$(echo "scale=2; $TOTAL_DOWNLOAD_SIZE / $TOTAL_DOWNLOAD_TIME" | bc)
    AVG_DOWNLOAD_MBPS=$(echo "scale=2; $AVG_DOWNLOAD_BYTES_PER_SEC * 8 / 1000000" | bc)
    echo -e "${GREEN}  Average Download Speed: ${AVG_DOWNLOAD_MBPS} Mbps${NC}"
fi
echo ""

# Upload speed test
echo -e "${YELLOW}[4/5] Testing upload speed...${NC}"
echo -e "${BLUE}  Testing with different file sizes:${NC}"

declare -a UPLOAD_SIZES=("1048576" "5242880")  # 1MB, 5MB
declare -a UPLOAD_LABELS=("1 MB" "5 MB")
TOTAL_UPLOAD_TIME=0
TOTAL_UPLOAD_SIZE=0

for i in "${!UPLOAD_SIZES[@]}"; do
    SIZE="${UPLOAD_SIZES[$i]}"
    LABEL="${UPLOAD_LABELS[$i]}"
    
    printf "${BLUE}  - Uploading ${LABEL}...${NC} "
    
    # Generate random data and upload
    START_TIME=$(date +%s.%N)
    dd if=/dev/zero bs="${SIZE}" count=1 2>/dev/null | \
    curl --max-time 120 --socks5-hostname "$SOCKS5_ADDR" -s \
    -X POST --data-binary @- \
    "$UPLOAD_URL" -o "$TMP_DIR/upload-result-${SIZE}.json" 2>/dev/null
    END_TIME=$(date +%s.%N)
    ELAPSED=$(echo "$END_TIME - $START_TIME" | bc)
    
    # Calculate speed in Mbps
    BYTES_PER_SEC=$(echo "scale=2; $SIZE / $ELAPSED" | bc)
    MBPS=$(echo "scale=2; $BYTES_PER_SEC * 8 / 1000000" | bc)
    
    printf "${GREEN}✓${NC} "
    printf "Time: %.3fs, Speed: ${GREEN}%.2f Mbps${NC}\n" "$ELAPSED" "$MBPS"
    
    TOTAL_UPLOAD_TIME=$(echo "$TOTAL_UPLOAD_TIME + $ELAPSED" | bc)
    TOTAL_UPLOAD_SIZE=$(echo "$TOTAL_UPLOAD_SIZE + $SIZE" | bc)
done

# Calculate average upload speed
if (( $(echo "$TOTAL_UPLOAD_TIME > 0" | bc -l) )); then
    AVG_UPLOAD_BYTES_PER_SEC=$(echo "scale=2; $TOTAL_UPLOAD_SIZE / $TOTAL_UPLOAD_TIME" | bc)
    AVG_UPLOAD_MBPS=$(echo "scale=2; $AVG_UPLOAD_BYTES_PER_SEC * 8 / 1000000" | bc)
    echo -e "${GREEN}  Average Upload Speed: ${AVG_UPLOAD_MBPS} Mbps${NC}"
fi
echo ""

# HTTPS latency test
echo -e "${YELLOW}[5/5] Testing HTTPS latency...${NC}"
echo -e "${BLUE}  Testing 5 consecutive HTTPS requests:${NC}"

TOTAL_HTTPS_TIME=0
for i in {1..5}; do
    printf "${BLUE}  - Request $i...${NC} "
    START_TIME=$(date +%s.%N)
    if curl --max-time 15 --socks5-hostname "$SOCKS5_ADDR" -s \
    https://httpbin.org/uuid > /dev/null 2>&1; then
        END_TIME=$(date +%s.%N)
        ELAPSED=$(echo "$END_TIME - $START_TIME" | bc)
        TOTAL_HTTPS_TIME=$(echo "$TOTAL_HTTPS_TIME + $ELAPSED" | bc)
        printf "${GREEN}✓${NC} "
        printf "Time: %.3fs\n" "$ELAPSED"
    else
        echo -e "${RED}✗ Failed${NC}"
    fi
    sleep 0.2
done

AVG_HTTPS_TIME=$(echo "scale=3; $TOTAL_HTTPS_TIME / 5" | bc)
echo -e "${GREEN}  Average HTTPS Latency: ${AVG_HTTPS_TIME}s${NC}"
echo ""

# Generate final report
echo -e "${BLUE}╔════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║                    Final Report                        ║${NC}"
echo -e "${BLUE}╠════════════════════════════════════════════════════════╣${NC}"
printf "${BLUE}║${NC} %-35s ${GREEN}%-17s${BLUE}║${NC}\n" "Metric" "Value"
echo -e "${BLUE}╠════════════════════════════════════════════════════════╣${NC}"
printf "${BLUE}║${NC} %-35s ${GREEN}%-17s${BLUE}║${NC}\n" "Basic HTTP Latency" "$(printf '%.3fs' $LATENCY)"
printf "${BLUE}║${NC} %-35s ${GREEN}%-17s${BLUE}║${NC}\n" "Average HTTPS Latency" "${AVG_HTTPS_TIME}s"
printf "${BLUE}║${NC} %-35s ${GREEN}%-17s${BLUE}║${NC}\n" "Average Download Speed" "${AVG_DOWNLOAD_MBPS} Mbps"
printf "${BLUE}║${NC} %-35s ${GREEN}%-17s${BLUE}║${NC}\n" "Average Upload Speed" "${AVG_UPLOAD_MBPS} Mbps"
echo -e "${BLUE}╠════════════════════════════════════════════════════════╣${NC}"

# Performance rating
DOWNLOAD_RATING=""
if (( $(echo "$AVG_DOWNLOAD_MBPS >= 50" | bc -l) )); then
    DOWNLOAD_RATING="${GREEN}Excellent${NC}"
    elif (( $(echo "$AVG_DOWNLOAD_MBPS >= 20" | bc -l) )); then
    DOWNLOAD_RATING="${YELLOW}Good${NC}"
else
    DOWNLOAD_RATING="${RED}Fair${NC}"
fi

UPLOAD_RATING=""
if (( $(echo "$AVG_UPLOAD_MBPS >= 10" | bc -l) )); then
    UPLOAD_RATING="${GREEN}Excellent${NC}"
    elif (( $(echo "$AVG_UPLOAD_MBPS >= 1" | bc -l) )); then
    UPLOAD_RATING="${YELLOW}Good${NC}"
else
    UPLOAD_RATING="${RED}Fair${NC}"
fi

printf "${BLUE}║${NC} %-35s %-23s${BLUE}║${NC}\n" "Download Performance" "$DOWNLOAD_RATING"
printf "${BLUE}║${NC} %-35s %-23s${BLUE}║${NC}\n" "Upload Performance" "$UPLOAD_RATING"
echo -e "${BLUE}╠════════════════════════════════════════════════════════╣${NC}"

# Usage recommendations
echo -e "${BLUE}║                 Usage Recommendations                  ║${NC}"
echo -e "${BLUE}╠════════════════════════════════════════════════════════╣${NC}"
if (( $(echo "$AVG_DOWNLOAD_MBPS >= 20" | bc -l) )); then
    echo -e "${BLUE}║${NC} ${GREEN}✓${NC} Suitable for web browsing and API calls          ${BLUE}║${NC}"
fi
if (( $(echo "$AVG_DOWNLOAD_MBPS >= 10" | bc -l) )); then
    echo -e "${BLUE}║${NC} ${GREEN}✓${NC} Suitable for file downloads                       ${BLUE}║${NC}"
fi
if (( $(echo "$AVG_UPLOAD_MBPS >= 1" | bc -l) )); then
    echo -e "${BLUE}║${NC} ${GREEN}✓${NC} Suitable for light data uploads                   ${BLUE}║${NC}"
fi
if (( $(echo "$AVG_UPLOAD_MBPS < 5" | bc -l) )); then
    echo -e "${BLUE}║${NC} ${YELLOW}!${NC} Upload speed limited - avoid large file uploads  ${BLUE}║${NC}"
fi
if (( $(echo "$LATENCY < 1" | bc -l) )); then
    echo -e "${BLUE}║${NC} ${GREEN}✓${NC} Low latency - excellent for interactive apps     ${BLUE}║${NC}"
fi
echo -e "${BLUE}╚════════════════════════════════════════════════════════╝${NC}"
echo ""

# Save report to file
REPORT_FILE="$TMP_DIR/speedtest-report-$(date +%Y%m%d-%H%M%S).txt"
cat > "$REPORT_FILE" << EOF
HttpBroker Speed Test Report
Generated: $(date)
SOCKS5 Proxy: $SOCKS5_ADDR

Performance Metrics:
--------------------
Basic HTTP Latency:        $(printf '%.3fs' $LATENCY)
Average HTTPS Latency:     ${AVG_HTTPS_TIME}s
Average Download Speed:    ${AVG_DOWNLOAD_MBPS} Mbps
Average Upload Speed:      ${AVG_UPLOAD_MBPS} Mbps

Test Details:
-------------
Download Tests: ${#SIZES[@]} files (${LABELS[*]})
Upload Tests:   ${#UPLOAD_SIZES[@]} files (${UPLOAD_LABELS[*]})
HTTPS Tests:    5 consecutive requests

Total Download Size: $(echo "scale=2; $TOTAL_DOWNLOAD_SIZE / 1048576" | bc) MB
Total Download Time: $(printf '%.3fs' $TOTAL_DOWNLOAD_TIME)
Total Upload Size:   $(echo "scale=2; $TOTAL_UPLOAD_SIZE / 1048576" | bc) MB
Total Upload Time:   $(printf '%.3fs' $TOTAL_UPLOAD_TIME)
EOF

echo -e "${GREEN}✓ Report saved to: ${REPORT_FILE}${NC}"
echo ""

# Cleanup
echo -e "${YELLOW}Cleaning up temporary files...${NC}"
rm -f "$TMP_DIR"/download-*.bin "$TMP_DIR"/upload-result-*.json
echo -e "${GREEN}✓ Cleanup complete${NC}"
echo ""

echo -e "${BLUE}Speed test completed successfully!${NC}"

