package deej

import (
	"bufio"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

const (
	// "pactl subscribe" gets restarted when it dies, with a growing delay
	pactlRestartMinDelay = 2 * time.Second
	pactlRestartMaxDelay = 30 * time.Second

	// a pactl run that lasted at least this long counts as healthy (resets the delay)
	pactlHealthyRunTime = 10 * time.Second
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	newSinkInputCh     chan uint32
	changedSinkInputCh chan uint32

	// pactlMu guards pactlCmd. stopCh is closed by Release and ends the subscribe loop
	pactlMu  sync.Mutex
	pactlCmd *exec.Cmd
	stopCh   chan struct{}
	stopOnce sync.Once
}

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	client, conn, err := proto.Connect("")
	if err != nil {
		logger.Warnw("Failed to establish PulseAudio connection", "error", err)
		return nil, fmt.Errorf("establish PulseAudio connection: %w", err)
	}

	request := proto.SetClientName{
		Props: proto.PropList{
			"application.name": proto.PropListString("deej"),
		},
	}
	reply := proto.SetClientNameReply{}

	if err := client.Request(&request, &reply); err != nil {
		return nil, err
	}

	sf := &paSessionFinder{
		logger:             logger.Named("session_finder"),
		sessionLogger:      logger.Named("sessions"),
		client:             client,
		conn:               conn,
		newSinkInputCh:     make(chan uint32, 16),
		changedSinkInputCh: make(chan uint32, 16),
		stopCh:             make(chan struct{}),
	}

	sf.startPactlSubscribe()

	sf.logger.Debug("Created PA session finder instance")
	return sf, nil
}

// startPactlSubscribe keeps a "pactl subscribe" subprocess running in the background to receive
// PulseAudio/PipeWire events. The native proto.Subscribe is blocked by PipeWire for clients
// without active streams, so we use pactl which always has the necessary permissions.
// If pactl dies (e.g. the audio server restarts) it's started again, until Release is called
func (sf *paSessionFinder) startPactlSubscribe() {
	go func() {
		delay := pactlRestartMinDelay

		for {
			started := time.Now()

			if err := sf.runPactlSubscribe(); err != nil {
				sf.logger.Warnw("pactl subscribe failed", "error", err)
			}

			// grow the delay while pactl keeps failing, so a missing binary doesn't spam the log
			if time.Since(started) >= pactlHealthyRunTime {
				delay = pactlRestartMinDelay
			} else {
				delay *= 2
				if delay > pactlRestartMaxDelay {
					delay = pactlRestartMaxDelay
				}
			}

			select {
			case <-sf.stopCh:
				return
			case <-time.After(delay):
				sf.logger.Debug("Restarting pactl subscribe")
			}
		}
	}()
}

// runPactlSubscribe runs one "pactl subscribe" process and blocks until it ends.
// Lines like "Event 'new' on sink-input #42" trigger a session refresh
func (sf *paSessionFinder) runPactlSubscribe() error {
	cmd := exec.Command("pactl", "subscribe")

	// put the subprocess in its own process group so we can kill it cleanly
	// without affecting deej itself when we send SIGKILL on release
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create pactl stdout pipe: %w", err)
	}

	// starting under the lock makes sure Release can't slip in between the
	// stop check and the start, and leave us with an orphaned process
	sf.pactlMu.Lock()

	select {
	case <-sf.stopCh:
		sf.pactlMu.Unlock()
		return nil
	default:
	}

	if err := cmd.Start(); err != nil {
		sf.pactlMu.Unlock()
		return fmt.Errorf("start pactl subscribe: %w", err)
	}

	sf.pactlCmd = cmd
	sf.pactlMu.Unlock()

	sf.logger.Debug("Started pactl subscribe for PulseAudio event monitoring")

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		sf.logger.Debugw("pactl event", "line", line)

		if !strings.Contains(line, "sink-input") {
			continue
		}

		if strings.Contains(line, "'new'") {
			sf.logger.Debugw("New sink input detected via pactl")
			select {
			case sf.newSinkInputCh <- 0: // index not needed, we do a full refresh
			default:
			}
		} else if strings.Contains(line, "'change'") {
			sf.logger.Debugw("Sink input change detected via pactl")
			select {
			case sf.changedSinkInputCh <- 0:
			default:
			}
		}
	}

	scanErr := scanner.Err()

	// reap the process (also frees the pipe)
	_ = cmd.Wait()

	sf.pactlMu.Lock()
	sf.pactlCmd = nil
	sf.pactlMu.Unlock()

	sf.logger.Debug("pactl subscribe process ended")

	return scanErr
}

func (sf *paSessionFinder) NewSinkInputChannel() <-chan uint32 {
	return sf.newSinkInputCh
}

func (sf *paSessionFinder) ChangedSinkInputChannel() <-chan uint32 {
	return sf.changedSinkInputCh
}

func (sf *paSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	masterSink, err := sf.getMasterSinkSession()
	if err == nil {
		sessions = append(sessions, masterSink)
	} else {
		sf.logger.Warnw("Failed to get master audio sink session", "error", err)
	}

	masterSource, err := sf.getMasterSourceSession()
	if err == nil {
		sessions = append(sessions, masterSource)
	} else {
		sf.logger.Warnw("Failed to get master audio source session", "error", err)
	}

	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio sessions", "error", err)
		return nil, fmt.Errorf("enumerate audio sessions: %w", err)
	}

	return sessions, nil
}

func (sf *paSessionFinder) Release() error {
	sf.stopOnce.Do(func() { close(sf.stopCh) })

	sf.pactlMu.Lock()
	if sf.pactlCmd != nil && sf.pactlCmd.Process != nil {
		// kill the entire process group to ensure pactl and any children exit.
		// the goroutine running it reaps the process
		pgid, err := syscall.Getpgid(sf.pactlCmd.Process.Pid)
		if err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = sf.pactlCmd.Process.Kill()
		}
	}
	sf.pactlMu.Unlock()

	if err := sf.conn.Close(); err != nil {
		sf.logger.Warnw("Failed to close PulseAudio connection", "error", err)
		return fmt.Errorf("close PulseAudio connection: %w", err)
	}

	sf.logger.Debug("Released PA session finder instance")
	return nil
}

func (sf *paSessionFinder) getMasterSinkSession() (Session, error) {
	request := proto.GetSinkInfo{SinkIndex: proto.Undefined}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master sink info", "error", err)
		return nil, fmt.Errorf("get master sink info: %w", err)
	}

	return newMasterSession(sf.sessionLogger, sf.client, reply.SinkIndex, reply.Channels, true), nil
}

func (sf *paSessionFinder) getMasterSourceSession() (Session, error) {
	request := proto.GetSourceInfo{SourceIndex: proto.Undefined}
	reply := proto.GetSourceInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master source info", "error", err)
		return nil, fmt.Errorf("get master source info: %w", err)
	}

	return newMasterSession(sf.sessionLogger, sf.client, reply.SourceIndex, reply.Channels, false), nil
}

// getSessionName resolves the best display name for a sink input.
// Priority: application.name → node.name → application.process.binary
// This covers standard apps (application.name), PipeWire/Java apps (node.name),
// and native Linux apps that only set process.binary.
func (sf *paSessionFinder) getSessionName(info *proto.GetSinkInputInfoReply) (string, bool) {
	for _, key := range []string{"application.name", "node.name", "application.process.binary"} {
		if val, ok := info.Properties[key]; ok {
			name := val.String()
			if name != "" {
				sf.logger.Debugw("Resolved session name",
					"key", key,
					"name", name,
					"sinkInputIndex", info.SinkInputIndex)
				return name, true
			}
		}
	}
	return "", false
}

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	for _, info := range reply {
		name, ok := sf.getSessionName(info)
		if !ok {
			sf.logger.Debugw("Skipping sink input: no usable name found",
				"sinkInputIndex", info.SinkInputIndex)
			continue
		}
		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name)
		*sessions = append(*sessions, newSession)
	}

	return nil
}
