package provider

import (
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// StreamHandler handles a single tunnel stream from the broker.
type StreamHandler struct {
	dialTimeout  time.Duration
	scrubHeaders bool
	logger       *zap.Logger
}

// NewStreamHandler creates a new StreamHandler.
func NewStreamHandler(
	dialTimeout time.Duration,
	scrubHeaders bool,
	logger *zap.Logger,
) *StreamHandler {
	return &StreamHandler{
		dialTimeout:  dialTimeout,
		scrubHeaders: scrubHeaders,
		logger:       logger,
	}
}

// Handle reads the target address, dials the target, and bridges the stream.
// This runs in its own goroutine per stream.
func (h *StreamHandler) Handle(stream net.Conn) {
	defer stream.Close()

	// Step 1: Read target address.
	// Format: 1 byte length + host:port string.
	addrLenBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, addrLenBuf); err != nil {
		h.logger.Error("failed to read address length from stream",
			zap.Error(err),
		)
		return
	}

	addrLen := int(addrLenBuf[0])
	addrBuf := make([]byte, addrLen)
	if _, err := io.ReadFull(stream, addrBuf); err != nil {
		h.logger.Error("failed to read address from stream",
			zap.Error(err),
		)
		return
	}
	addr := string(addrBuf)

	h.logger.Debug("handling stream",
		zap.String("target", addr),
	)

	// Step 2: Dial target.
	targetConn, err := net.DialTimeout("tcp", addr, h.dialTimeout)
	if err != nil {
		h.logger.Error("failed to dial target",
			zap.String("target", addr),
			zap.Error(err),
		)
		return
	}
	defer targetConn.Close()

	// Step 3: Optionally wrap target connection with scrubber.
	var targetWriter io.Writer = targetConn
	if h.scrubHeaders {
		targetWriter = NewScrubConn(targetConn)
	}

	// Step 4: Bridge bidirectionally.
	var wg sync.WaitGroup
	wg.Add(2)

	// stream → target (data from tunnel written to target, possibly scrubbed)
	go func() {
		defer wg.Done()
		io.Copy(targetWriter, stream)
		// Signal target that no more data is coming from this direction.
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// target → stream (data from target written back to tunnel)
	go func() {
		defer wg.Done()
		io.Copy(stream, targetConn)
		// Signal stream that no more data is coming from this direction.
		if closeWriter, ok := stream.(interface{ CloseWrite() error }); ok {
			closeWriter.CloseWrite()
		}
	}()

	// Wait for both directions to finish.
	wg.Wait()

	h.logger.Debug("stream completed",
		zap.String("target", addr),
	)
}
