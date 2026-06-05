package deej

import (
	"bufio"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

const (
	paMaskSinkInput     uint32 = 0x0008
	paEventChange       uint32 = 0x0010
	paEventSinkInput    uint32 = 0x0002
	paEventNew          uint32 = 0x0000
	paEventFacilityMask uint32 = 0x000F
	paEventTypeMask     uint32 = 0x0030
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	newSinkInputCh     chan uint32
	changedSinkInputCh chan uint32

	pactlCmd *exec.Cmd
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
	}

	// Start pactl subscribe as a subprocess to receive PulseAudio/PipeWire events.
	// The native proto.Subscribe is blocked by PipeWire for clients without active
	// streams, so we use pactl which always has the necessary permissions.
	sf.startPactlSubscribe()

	sf.logger.Debug("Created PA session finder instance")
	return sf, nil
}

// startPactlSubscribe launches "pactl subscribe" and parses its output line by line.
// Lines like "Event 'new' on sink-input #42" trigger a session refresh.
func (sf *paSessionFinder) startPactlSubscribe() {
	cmd := exec.Command("pactl", "subscribe")
	// Put the subprocess in its own process group so we can kill it cleanly
	// without affecting deej itself when we send SIGKILL on release.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sf.pactlCmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sf.logger.Warnw("Failed to create pactl stdout pipe, falling back to polling", "error", err)
		return
	}

	if err := cmd.Start(); err != nil {
		sf.logger.Warnw("Failed to start pactl subscribe, falling back to polling", "error", err)
		return
	}

	sf.logger.Debug("Started pactl subscribe for PulseAudio event monitoring")

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			sf.logger.Debugw("pactl event", "line", line)

			isSinkInput := strings.Contains(line, "sink-input")
			isNew := strings.Contains(line, "'new'")
			isChange := strings.Contains(line, "'change'")

			if !isSinkInput {
				continue
			}

			if isNew {
				sf.logger.Debugw("New sink input detected via pactl")
				select {
				case sf.newSinkInputCh <- 0: // index not needed, we do a full refresh
				default:
				}
			} else if isChange {
				sf.logger.Debugw("Sink input change detected via pactl")
				select {
				case sf.changedSinkInputCh <- 0:
				default:
				}
			}
		}

		if err := scanner.Err(); err != nil {
			sf.logger.Warnw("pactl subscribe scanner error", "error", err)
		}

		sf.logger.Debug("pactl subscribe process ended")
	}()
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
	if sf.pactlCmd != nil && sf.pactlCmd.Process != nil {
		// Kill the entire process group to ensure pactl and any children exit
		pgid, err := syscall.Getpgid(sf.pactlCmd.Process.Pid)
		if err == nil {
			syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			sf.pactlCmd.Process.Kill()
		}
		sf.pactlCmd.Wait()
	}

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

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	for _, info := range reply {
		name, ok := info.Properties["application.name"]
		if !ok {
			sf.logger.Debugw("Skipping sink input without process name",
				"sinkInputIndex", info.SinkInputIndex)
			continue
		}
		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name.String())
		*sessions = append(*sessions, newSession)
	}

	return nil
}
