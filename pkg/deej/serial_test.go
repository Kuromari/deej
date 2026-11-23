package deej

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestSerialIO() *SerialIO {
	return &SerialIO{
		deej:   &Deej{config: &CanonicalConfig{NoiseReductionLevel: "default"}},
		logger: zap.NewNop().Sugar(),
	}
}

func TestParseSliderLine(t *testing.T) {
	logger := zap.NewNop().Sugar()

	tests := []struct {
		name       string
		line       string
		wantValues map[int]float32 // slider -> expected value; nil means "line must be ignored"
	}{
		{"regular line", "1023|0|512\r\n", map[int]float32{0: 1.0, 1: 0.0, 2: 0.5}},
		{"1024 is clamped, not dropped (end of travel)", "1024|0\r\n", map[int]float32{0: 1.0, 1: 0.0}},
		{"1024 on another slider", "0|1024\r\n", map[int]float32{0: 0.0, 1: 1.0}},
		{"dirty first value", "4558|925|41|643|220\r\n", nil},
		{"dirty value on a later slider", "5|2000\r\n", nil},
		{"garbage", "abc\r\n", nil},
		{"missing CR", "1|2\n", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sio := newTestSerialIO()
			events := sio.parseSliderLine(logger, make(chan struct{}), tt.line)

			if tt.wantValues == nil {
				if len(events) != 0 {
					t.Fatalf("expected line to be ignored, got events %v", events)
				}
				return
			}

			got := map[int]float32{}
			for _, e := range events {
				got[e.SliderID] = e.PercentValue
			}

			for id, want := range tt.wantValues {
				if v, ok := got[id]; !ok || v != want {
					t.Errorf("slider %d: got %v (present=%v), want %v", id, v, ok, want)
				}
			}
		})
	}
}

func TestParseSliderLineStaleConnection(t *testing.T) {
	sio := newTestSerialIO()
	done := make(chan struct{})
	close(done)

	if events := sio.parseSliderLine(zap.NewNop().Sugar(), done, "10|20\r\n"); len(events) != 0 {
		t.Fatalf("stale line must be dropped, got %v", events)
	}
}

// a consumer that never reads must not be able to block anything that needs lineMu
func TestSlowConsumerDoesNotHoldLineLock(t *testing.T) {
	sio := newTestSerialIO()
	sio.sliderMoveConsumers = []chan SliderMoveEvent{make(chan SliderMoveEvent)} // unbuffered, nobody reads

	done := make(chan struct{})
	handled := make(chan struct{})

	go func() {
		sio.handleLine(zap.NewNop().Sugar(), done, "500|600\r\n")
		close(handled)
	}()

	// handleLine is now stuck delivering. NumSliders needs lineMu and must still answer
	answered := make(chan int, 1)
	go func() {
		// give handleLine a moment to reach the delivery
		time.Sleep(50 * time.Millisecond)
		answered <- sio.NumSliders()
	}()

	select {
	case n := <-answered:
		if n != 2 {
			t.Errorf("NumSliders = %d, want 2", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lineMu is held while delivering events to a slow consumer")
	}

	// tearing the connection down must release the stuck delivery
	close(done)

	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("handleLine didn't return after the connection was torn down")
	}
}
