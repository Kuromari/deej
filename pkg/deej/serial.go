package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

const (
	// how often we ping the board (only boards that answered our handshake)
	heartbeatInterval = 2 * time.Second

	// reconnect if the board stopped answering pings for this long
	heartbeatTimeout = 6 * time.Second

	// reconnect if nothing at all (no slider data, no answers) arrived for this long.
	// this is the fallback for boards running a sketch without heartbeat support
	silenceTimeout = 5 * time.Second

	watchdogTick = time.Second

	// pings without an answer after which we assume the sketch has no heartbeat support
	heartbeatUnsupportedAfter = 5

	reconnectMinDelay = 500 * time.Millisecond
	reconnectMaxDelay = 5 * time.Second
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	comPort  string
	baudRate uint

	deej   *Deej
	logger *zap.SugaredLogger

	connOptions serial.OpenOptions

	// stateMu guards conn, connDone, connID, userStopped and reconnecting.
	// lock order: writeMutex -> stateMu (never the other way around)
	stateMu      sync.Mutex
	conn         io.ReadWriteCloser
	connDone     chan struct{} // closed when the current connection is torn down
	connID       uint64        // incremented on every successful open, so stale goroutines can tell
	userStopped  bool          // Stop() was called: don't reconnect on our own
	reconnecting bool

	connected int32 // atomic mirror of (conn != nil), for lock-free reads

	// lineMu serializes line handling, so a reader that's still finishing its last
	// line after a reconnect can't race with the new connection's reader
	lineMu                     sync.Mutex
	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent

	// two-way communication: writes are serialized, and the board announces itself with "READY"
	writeMutex      sync.Mutex
	boardReady      int32
	boardReadyHooks []func()

	// liveness tracking, as nanoseconds since epoch (monotonic, immune to wall clock jumps)
	epoch            time.Time
	lastRx           int64 // last line of any kind received
	lastHeartbeatAck int64 // last "HB" received
	heartbeatSeen    int32 // board has answered at least one heartbeat on this connection
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

var expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*\r\n$`)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		epoch:               time.Now(),
		sliderMoveConsumers: []chan SliderMoveEvent{},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// nowNanos returns a monotonic timestamp for the liveness counters
func (sio *SerialIO) nowNanos() int64 {
	return int64(time.Since(sio.epoch))
}

// Start attempts to connect to our arduino chip
func (sio *SerialIO) Start() error {
	sio.stateMu.Lock()
	defer sio.stateMu.Unlock()

	// don't allow multiple concurrent connections
	if sio.conn != nil {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	// an explicit Start means we want to be connected (again)
	sio.userStopped = false

	if err := sio.openLocked(); err != nil {
		// might need a user notification here, TBD
		sio.logger.Warnw("Failed to open serial connection", "error", err)
		return err
	}

	return nil
}

// openLocked opens the port and spawns the goroutines that serve this connection.
// must be called with sio.stateMu held and no active connection
func (sio *SerialIO) openLocked() error {

	// set minimum read size according to platform (0 for windows, 1 for linux)
	// this prevents a rare bug on windows where serial reads get congested,
	// resulting in significant lag
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	sio.connOptions = serial.OpenOptions{
		PortName:        sio.deej.config.ConnectionInfo.COMPort,
		BaudRate:        uint(sio.deej.config.ConnectionInfo.BaudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: uint(minimumReadSize),
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", sio.connOptions.PortName,
		"baudRate", sio.connOptions.BaudRate,
		"minReadSize", minimumReadSize)

	conn, err := serial.Open(sio.connOptions)
	if err != nil {
		return fmt.Errorf("open serial connection: %w", err)
	}

	namedLogger := sio.logger.Named(strings.ToLower(sio.connOptions.PortName))
	namedLogger.Infow("Connected", "conn", conn)

	sio.connID++
	id := sio.connID
	done := make(chan struct{})

	sio.conn = conn
	sio.connDone = done

	atomic.StoreInt32(&sio.connected, 1)
	atomic.StoreInt32(&sio.boardReady, 0)
	atomic.StoreInt32(&sio.heartbeatSeen, 0)

	now := sio.nowNanos()
	atomic.StoreInt64(&sio.lastRx, now)
	atomic.StoreInt64(&sio.lastHeartbeatAck, now)

	// make the next line re-emit slider move events for all sliders, so volumes
	// are re-applied after a reconnect
	sio.lineMu.Lock()
	sio.lastKnownNumSliders = 0
	sio.lineMu.Unlock()

	// ping the board until it answers, so it can get its LED colors (see handshake)
	go sio.handshake(namedLogger, id)

	// read lines until the connection dies
	go sio.readLoop(namedLogger, id, conn, done)

	// keep checking that the board is still alive, and reconnect if it isn't
	go sio.heartbeatLoop(namedLogger, id, done)
	go sio.watchdogLoop(namedLogger, id, done)

	return nil
}

// isCurrent returns whether id belongs to the connection that's open right now
func (sio *SerialIO) isCurrent(id uint64) bool {
	sio.stateMu.Lock()
	defer sio.stateMu.Unlock()

	return sio.conn != nil && sio.connID == id
}

// teardown closes the connection identified by id. Returns false if that
// connection was already gone (or replaced by a newer one), so only one
// caller ever gets to handle a given connection's death
func (sio *SerialIO) teardown(id uint64) bool {
	sio.stateMu.Lock()

	if sio.conn == nil || sio.connID != id {
		sio.stateMu.Unlock()
		return false
	}

	conn := sio.conn
	done := sio.connDone

	sio.conn = nil
	sio.connDone = nil

	atomic.StoreInt32(&sio.connected, 0)
	atomic.StoreInt32(&sio.boardReady, 0)

	sio.stateMu.Unlock()

	// wake up everything that waits on this connection
	close(done)

	// deliberately not holding writeMutex here: a write stuck on a dead port
	// must not be able to block the close that would free it
	if err := conn.Close(); err != nil {
		sio.logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		sio.logger.Debug("Serial connection closed")
	}

	return true
}

// connectionLost is called by any goroutine that finds the connection broken or
// unresponsive. It closes the connection and starts reconnecting (unless deej is
// shutting the connection down on purpose)
func (sio *SerialIO) connectionLost(id uint64, reason string) {
	if !sio.teardown(id) {
		return
	}

	sio.logger.Warnw("Serial connection lost, will try to reconnect", "reason", reason)

	sio.stateMu.Lock()
	start := !sio.userStopped && !sio.reconnecting
	if start {
		sio.reconnecting = true
	}
	sio.stateMu.Unlock()

	if start {
		go sio.reconnectLoop()
	}
}

// reconnectLoop retries opening the port with a growing delay until it works,
// or until someone stops us / connects on their own
func (sio *SerialIO) reconnectLoop() {
	delay := reconnectMinDelay

	for attempt := 1; ; attempt++ {
		time.Sleep(delay)

		sio.stateMu.Lock()

		if sio.userStopped || sio.conn != nil {
			sio.reconnecting = false
			sio.stateMu.Unlock()
			return
		}

		err := sio.openLocked()
		if err == nil {
			// must be reset while still holding the lock: the new connection's
			// goroutines may already be trying to report it as lost
			sio.reconnecting = false
			sio.stateMu.Unlock()

			sio.logger.Infow("Reconnected to board", "attempts", attempt)
			return
		}

		sio.stateMu.Unlock()

		if attempt == 1 || attempt%10 == 0 {
			sio.logger.Warnw("Reconnect attempt failed", "attempt", attempt, "error", err)
		}

		delay *= 2
		if delay > reconnectMaxDelay {
			delay = reconnectMaxDelay
		}
	}
}

// Write sends a raw line to the board. Safe to call from multiple goroutines
func (sio *SerialIO) Write(data string) error {
	sio.writeMutex.Lock()
	defer sio.writeMutex.Unlock()

	sio.stateMu.Lock()
	conn := sio.conn
	sio.stateMu.Unlock()

	if conn == nil {
		return errors.New("serial: not connected")
	}

	if _, err := conn.Write([]byte(data)); err != nil {
		return fmt.Errorf("serial write: %w", err)
	}

	return nil
}

// Connected returns whether a serial connection is currently open
func (sio *SerialIO) Connected() bool {
	return atomic.LoadInt32(&sio.connected) == 1
}

// BoardReady returns whether the board has answered our handshake on this connection
func (sio *SerialIO) BoardReady() bool {
	return atomic.LoadInt32(&sio.boardReady) == 1
}

// OnBoardReady registers a function that's called whenever the board says it's
// ready (after boot, or as an answer to our ping). Must be called before Start
func (sio *SerialIO) OnBoardReady(hook func()) {
	sio.boardReadyHooks = append(sio.boardReadyHooks, hook)
}

// handshake pings the board until it answers with READY. Opening the port
// resets some boards (Uno/Nano) and swallows whatever we send during boot, so
// a single blind write wouldn't be reliable. Boards that run the vanilla
// sketch never answer; the pings are harmless and we give up after a while
func (sio *SerialIO) handshake(logger *zap.SugaredLogger, id uint64) {
	const (
		attempts = 20
		interval = 500 * time.Millisecond
	)

	for i := 0; i < attempts; i++ {
		if sio.BoardReady() || !sio.isCurrent(id) {
			return
		}

		if err := sio.Write("?\n"); err != nil {
			logger.Debugw("Failed to send handshake ping", "error", err)
			return
		}

		time.Sleep(interval)
	}

	if !sio.BoardReady() && sio.isCurrent(id) {
		logger.Info("Board didn't answer the handshake - it's probably running a sketch without LED support")
	}
}

// heartbeatLoop sends "H" to the board every few seconds; the board answers "HB".
// Only boards that answered the handshake (i.e. run the LED sketch, which reads
// serial input) get pings: a board that never reads its input would eventually
// block our writes once its receive buffer is full
func (sio *SerialIO) heartbeatLoop(logger *zap.SugaredLogger, id uint64, done chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	unanswered := 0

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		if !sio.BoardReady() {
			continue
		}

		if atomic.LoadInt32(&sio.heartbeatSeen) == 0 {
			unanswered++

			if unanswered == heartbeatUnsupportedAfter {
				logger.Info("Board doesn't answer heartbeats - its sketch is probably older. " +
					"Flash the current sketch to get the full check, until then only incoming data is monitored")
			}
		}

		if err := sio.Write("H\n"); err != nil {
			sio.connectionLost(id, fmt.Sprintf("heartbeat write failed: %v", err))
			return
		}
	}
}

// watchdogLoop declares the connection dead when the board went quiet or stopped
// answering heartbeats
func (sio *SerialIO) watchdogLoop(logger *zap.SugaredLogger, id uint64, done chan struct{}) {
	ticker := time.NewTicker(watchdogTick)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		now := sio.nowNanos()

		silent := time.Duration(now - atomic.LoadInt64(&sio.lastRx))
		if silent > silenceTimeout {
			sio.connectionLost(id, fmt.Sprintf("no data from board for %s", silent.Round(time.Millisecond)))
			return
		}

		// once the board has proven that it answers heartbeats, insist on it:
		// this also catches a board that still streams values but no longer reacts
		if atomic.LoadInt32(&sio.heartbeatSeen) == 1 {
			sinceAck := time.Duration(now - atomic.LoadInt64(&sio.lastHeartbeatAck))
			if sinceAck > heartbeatTimeout {
				sio.connectionLost(id, fmt.Sprintf("no heartbeat answer for %s", sinceAck.Round(time.Millisecond)))
				return
			}
		}
	}
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	sio.stateMu.Lock()
	sio.userStopped = true // also cancels a reconnect loop that's currently running
	id := sio.connID
	active := sio.conn != nil
	sio.stateMu.Unlock()

	if !active {
		sio.logger.Debug("Not currently connected, nothing to stop")
		return
	}

	sio.logger.Debug("Shutting down serial connection")
	sio.teardown(id)
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for range configReloadedChannel {
			// make any config reload unset our slider number to ensure process volumes are being re-set
			// (the next read line will emit SliderMoveEvent instances for all sliders)\
			// this needs to happen after a small delay, because the session map will also re-acquire sessions
			// whenever the config file is reloaded, and we don't want it to receive these move events while the map
			// is still cleared. this is kind of ugly, but shouldn't cause any issues
			go func() {
				<-time.After(stopDelay)
				sio.lastKnownNumSliders = 0
			}()

			// if connection params have changed, attempt to stop and start the connection
			if sio.deej.config.ConnectionInfo.COMPort != sio.connOptions.PortName ||
				uint(sio.deej.config.ConnectionInfo.BaudRate) != sio.connOptions.BaudRate {

				sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
				sio.Stop()

				// let the connection close
				<-time.After(stopDelay)

				if err := sio.Start(); err != nil {
					sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
				} else {
					sio.logger.Debug("Renewed connection successfully")
				}
			}
		}
	}()
}

// readLoop reads lines from the board until the connection ends
func (sio *SerialIO) readLoop(logger *zap.SugaredLogger, id uint64, conn io.Reader, done chan struct{}) {
	reader := bufio.NewReader(conn)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {

			// closed on purpose (Stop, or someone else already handled the loss)
			select {
			case <-done:
				return
			default:
			}

			if sio.deej.Verbose() {
				logger.Warnw("Failed to read line from serial", "error", err, "line", line)
			}

			sio.connectionLost(id, fmt.Sprintf("read failed: %v", err))
			return
		}

		atomic.StoreInt64(&sio.lastRx, sio.nowNanos())

		if sio.deej.Verbose() {
			logger.Debugw("Read new line", "line", line)
		}

		sio.handleLine(logger, done, line)
	}
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, done chan struct{}, line string) {
	sio.lineMu.Lock()
	defer sio.lineMu.Unlock()

	// a line that was read just before its connection got torn down is stale
	select {
	case <-done:
		return
	default:
	}

	switch strings.TrimSpace(line) {

	// the board answers our heartbeat pings with HB
	case "HB":
		atomic.StoreInt64(&sio.lastHeartbeatAck, sio.nowNanos())
		atomic.StoreInt32(&sio.heartbeatSeen, 1)
		return

	// the board announces itself on boot and answers our handshake pings with READY
	case "READY":
		logger.Info("Board is ready")
		atomic.StoreInt32(&sio.boardReady, 1)

		for _, hook := range sio.boardReadyHooks {
			go hook()
		}

		return
	}

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	if !expectedLinePattern.MatchString(line) {
		return
	}

	// trim the suffix
	line = strings.TrimSuffix(line, "\r\n")

	// split on pipe (|), this gives a slice of numerical strings between "0" and "1023"
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)

		// turns out the first line could come out dirty sometimes (i.e. "4558|925|41|643|220")
		// so let's check the first number for correctness just in case
		if sliderIdx == 0 && number > 1023 {
			sio.logger.Debugw("Got malformed line from serial, ignoring", "line", line)
			return
		}

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / 1023.0

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if sio.deej.config.InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	// deliver move events if there are any, towards all potential consumers
	// (bail out if the connection is torn down meanwhile, so a slow consumer
	// can't keep a dead connection's reader stuck)
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				select {
				case consumer <- moveEvent:
				case <-done:
					return
				}
			}
		}
	}
}
