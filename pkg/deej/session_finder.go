package deej

// SessionFinder represents an entity that can find all current audio sessions
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	Release() error

	// NewSinkInputChannel returns a channel that emits the PulseAudio sink-input
	// index every time a new audio session appears. Returns nil on non-Linux
	// platforms or when event subscription is unavailable.
	NewSinkInputChannel() <-chan uint32
	ChangedSinkInputChannel() <-chan uint32 // neu
}
