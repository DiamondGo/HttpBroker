package broker

import (
	"io"
	"net"

	"github.com/DiamondGo/HttpBroker/internal/transport"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"
)

// Relay manages yamux sessions and bridges streams between consumers and providers.
type Relay struct {
	registry *EndpointRegistry
	logger   *zap.Logger
}

// NewRelay creates a new Relay with the given registry and logger.
func NewRelay(registry *EndpointRegistry, logger *zap.Logger) *Relay {
	return &Relay{
		registry: registry,
		logger:   logger,
	}
}

// HandleProvider sets up yamux on the provider session and registers it.
// Blocks until the provider disconnects, then closes all consumer yamux sessions
// for this endpoint so consumers detect the failure immediately.
func (r *Relay) HandleProvider(session *transport.Session) {
	consumerCount := r.registry.ConsumerCount(session.Endpoint)
	r.logger.Info("provider connected",
		zap.String("session_id", session.ID),
		zap.String("endpoint", session.Endpoint),
		zap.Int("active_consumers", consumerCount),
	)

	// Provider connection: broker is yamux client (opens streams TO provider).
	// Disable keepalives: the HTTP polling transport cannot reliably complete a
	// PING-PONG round-trip within yamux's ConnectionWriteTimeout (10s) when a
	// long-poll is in flight. Provider reconnects automatically on failure.
	yamuxCfg := yamux.DefaultConfig()
	yamuxCfg.EnableKeepAlive = false
	yamuxCfg.LogOutput = io.Discard
	yamuxSess, err := yamux.Client(session, yamuxCfg)
	if err != nil {
		r.logger.Error("failed to create yamux client session for provider",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
		return
	}
	defer func() {
		yamuxSess.Close()
		// RemoveProvider closes all consumer yamux sessions for this endpoint,
		// causing each consumer to detect the disconnection and re-register.
		// It also unblocks any bridgeStream goroutines waiting in WaitForProvider.
		r.registry.RemoveProvider(session.Endpoint)
		r.logger.Info("provider disconnected — notified all consumers to reconnect",
			zap.String("session_id", session.ID),
			zap.String("endpoint", session.Endpoint),
		)
	}()

	// Register the provider with its yamux session.
	if err := r.registry.SetProvider(session.Endpoint, session, yamuxSess); err != nil {
		r.logger.Error("failed to register provider",
			zap.String("session_id", session.ID),
			zap.String("endpoint", session.Endpoint),
			zap.Error(err),
		)
		return
	}

	// Notify any bridgeStream goroutines waiting for a provider to arrive.
	r.registry.NotifyProviderArrived(session.Endpoint)

	// Block until the yamux session closes (provider disconnects).
	<-yamuxSess.CloseChan()
}

// HandleConsumer sets up yamux on the consumer session and starts accepting streams.
// Blocks until the consumer disconnects.
func (r *Relay) HandleConsumer(session *transport.Session) {
	hasProvider := r.registry.HasProvider(session.Endpoint)
	r.logger.Info("consumer connected",
		zap.String("session_id", session.ID),
		zap.String("endpoint", session.Endpoint),
		zap.Bool("provider_available", hasProvider),
	)
	if !hasProvider {
		r.logger.Warn(
			"consumer connected but no provider is available yet — streams will wait indefinitely for provider",
			zap.String("endpoint", session.Endpoint),
		)
	}

	// Consumer connection: broker is yamux server (accepts streams FROM consumer).
	// Disable keepalives for the same reason as the provider session above.
	yamuxCfgC := yamux.DefaultConfig()
	yamuxCfgC.EnableKeepAlive = false
	yamuxCfgC.LogOutput = io.Discard
	r.logger.Debug("creating yamux server session for consumer")
	yamuxSess, err := yamux.Server(session, yamuxCfgC)
	if err != nil {
		r.logger.Error("failed to create yamux server session for consumer",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
		return
	}

	// Register the consumer yamux session so it can be closed when the provider
	// disconnects, allowing the consumer to detect the failure immediately.
	r.registry.RegisterConsumerYamux(session.Endpoint, session.ID, yamuxSess)

	defer func() {
		yamuxSess.Close()
		r.registry.UnregisterConsumerYamux(session.Endpoint, session.ID)
		r.logger.Info("consumer disconnected",
			zap.String("session_id", session.ID),
			zap.String("endpoint", session.Endpoint),
		)
	}()

	// sessionDone is closed when this consumer's yamux session ends.
	// Passed to bridgeStream so WaitForProvider can abort if the consumer leaves.
	sessionDone := yamuxSess.CloseChan()

	// Accept streams from the consumer and bridge them to the provider.
	for {
		r.logger.Debug("waiting for consumer stream (yamux accept)")
		stream, err := yamuxSess.Accept()
		if err != nil {
			if err != io.EOF {
				r.logger.Debug("consumer yamux accept error",
					zap.String("session_id", session.ID),
					zap.Error(err),
				)
			}
			return
		}

		r.logger.Debug("consumer stream accepted, bridging to provider")
		go r.bridgeStream(stream, session.Endpoint, sessionDone)
	}
}

// bridgeStream handles a single consumer yamux stream:
//  1. Read target address from stream (format: 1 byte length + host:port string)
//  2. Wait for a provider yamux session — indefinitely, but cancels if the
//     consumer yamux session closes (sessionDone fires). This handles the
//     "consumer connects before provider" case: the browser request is held
//     until the provider arrives, rather than failing immediately.
//  3. Open a new yamux stream to the provider
//  4. Write target address to the provider stream
//  5. Bridge with bidirectional io.Copy
func (r *Relay) bridgeStream(
	consumerStream net.Conn,
	endpointName string,
	sessionDone <-chan struct{},
) {
	defer consumerStream.Close()

	// Step 1: Read target address from consumer stream.
	addrLenBuf := make([]byte, 1)
	if _, err := io.ReadFull(consumerStream, addrLenBuf); err != nil {
		r.logger.Error("failed to read address length from consumer stream",
			zap.String("endpoint", endpointName),
			zap.Error(err),
		)
		return
	}

	addrLen := int(addrLenBuf[0])
	addrBuf := make([]byte, addrLen)
	if _, err := io.ReadFull(consumerStream, addrBuf); err != nil {
		r.logger.Error("failed to read address from consumer stream",
			zap.String("endpoint", endpointName),
			zap.Error(err),
		)
		return
	}
	targetAddr := string(addrBuf)

	r.logger.Debug("bridging stream",
		zap.String("endpoint", endpointName),
		zap.String("target", targetAddr),
	)

	// Step 2: Wait for a provider yamux session.
	// WaitForProvider blocks until a provider registers or sessionDone fires.
	// If the consumer disconnects (or the broker closes the consumer's yamux
	// session because the provider left), sessionDone fires and we abort.
	providerYamux, ok := r.registry.WaitForProvider(endpointName, sessionDone)
	if !ok {
		r.logger.Debug("consumer session ended while waiting for provider — stream dropped",
			zap.String("endpoint", endpointName),
			zap.String("target", targetAddr),
		)
		return
	}

	// Step 3: Open a new yamux stream to the provider.
	providerStream, err := providerYamux.Open()
	if err != nil {
		r.logger.Error("failed to open stream to provider",
			zap.String("endpoint", endpointName),
			zap.String("target", targetAddr),
			zap.Error(err),
		)
		return
	}
	defer providerStream.Close()

	// Step 4: Write target address to the provider stream.
	header := make([]byte, 1+addrLen)
	header[0] = byte(addrLen)
	copy(header[1:], addrBuf)
	if _, err := providerStream.Write(header); err != nil {
		r.logger.Error("failed to write address to provider stream",
			zap.String("endpoint", endpointName),
			zap.String("target", targetAddr),
			zap.Error(err),
		)
		return
	}

	// Step 5: Bridge with bidirectional io.Copy.
	errCh := make(chan error, 2)

	go func() {
		_, err := io.Copy(providerStream, consumerStream)
		// Close write side of provider stream to signal EOF.
		if closeWriter, ok := providerStream.(interface{ CloseWrite() error }); ok {
			closeWriter.CloseWrite()
		}
		errCh <- err
	}()

	go func() {
		_, err := io.Copy(consumerStream, providerStream)
		// Close write side of consumer stream to signal EOF.
		if closeWriter, ok := consumerStream.(interface{ CloseWrite() error }); ok {
			closeWriter.CloseWrite()
		}
		errCh <- err
	}()

	// Wait for both directions to finish.
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil && err != io.EOF {
			r.logger.Debug("bridge stream copy error",
				zap.String("endpoint", endpointName),
				zap.String("target", targetAddr),
				zap.Error(err),
			)
		}
	}

	r.logger.Debug("bridge stream completed",
		zap.String("endpoint", endpointName),
		zap.String("target", targetAddr),
	)
}
