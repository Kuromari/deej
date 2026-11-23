package deej

import (
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type fakeTransport struct {
	mu        sync.Mutex
	writes    []string
	connected bool
	block     chan struct{} // if set, Write waits until it's closed
}

func (f *fakeTransport) Write(data string) error {
	if f.block != nil {
		<-f.block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, data)

	return nil
}

func (f *fakeTransport) Connected() bool  { return f.connected }
func (f *fakeTransport) BoardReady() bool { return f.connected }

func (f *fakeTransport) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := f.writes
	f.writes = nil

	return out
}

func newTestController(t *testing.T, transport ledTransport) *LEDController {
	t.Helper()

	return &LEDController{
		logger:         zap.NewNop().Sugar(),
		transport:      transport,
		filepath:       filepath.Join(t.TempDir(), "leds.json"),
		sentBrightness: -1,
		sentLines:      map[int]string{},
		settings:       padLEDSettings(LEDSettings{Brightness: 100}, 2),
	}
}

func TestLEDLine(t *testing.T) {
	got := ledLine(2, LEDSlider{Stops: []string{"#ff0000", "#0000ff"}})
	if want := "L2:ff0000,0000ff"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSanitizeLEDSettings(t *testing.T) {
	one := []LEDSlider{{Stops: []string{"#AbCdEf"}}}

	tests := []struct {
		name    string
		in      LEDSettings
		wantErr string
	}{
		{"valid", LEDSettings{Brightness: 10, Sliders: one}, ""},
		{"brightness too high", LEDSettings{Brightness: 256, Sliders: one}, "brightness"},
		{"brightness negative", LEDSettings{Brightness: -1, Sliders: one}, "brightness"},
		{"no sliders", LEDSettings{Brightness: 10}, "number of sliders"},
		{"too many sliders", LEDSettings{Brightness: 10, Sliders: make([]LEDSlider, ledMaxSliders+1)}, "number of sliders"},
		{"no stops", LEDSettings{Brightness: 10, Sliders: []LEDSlider{{}}}, "expected 1 to"},
		{"too many stops", LEDSettings{Brightness: 10, Sliders: []LEDSlider{{Stops: make([]string, ledMaxStops+1)}}}, "expected 1 to"},
		{"invalid color", LEDSettings{Brightness: 10, Sliders: []LEDSlider{{Stops: []string{"red"}}}}, "invalid color"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, err := sanitizeLEDSettings(tt.in)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := clean.Sliders[0].Stops[0]; got != "#abcdef" {
					t.Errorf("color not lowercased: %q", got)
				}
				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPushOnlySendsChanges(t *testing.T) {
	tr := &fakeTransport{connected: true}
	c := newTestController(t, tr)

	// first push sends everything
	c.PushAll()
	want := []string{"B100\n", "L0:ff3b30\n", "L1:ff9500\n"}
	if got := tr.take(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first push: got %q, want %q", got, want)
	}

	// applying identical settings sends nothing
	if err := c.Apply(c.Snapshot(2), false); err != nil {
		t.Fatal(err)
	}
	if got := tr.take(); len(got) != 0 {
		t.Fatalf("unchanged settings were sent again: %q", got)
	}

	// changing one slider sends only that one
	changed := c.Snapshot(2)
	changed.Sliders[1].Stops = []string{"#000000"}
	if err := c.Apply(changed, false); err != nil {
		t.Fatal(err)
	}
	if got, want := tr.take(), []string{"L1:000000\n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplySavesToDisk(t *testing.T) {
	c := newTestController(t, &fakeTransport{})

	if err := c.Apply(c.Snapshot(2), true); err != nil {
		t.Fatal(err)
	}

	loaded := c.loadFromDisk()
	if !reflect.DeepEqual(loaded, c.Snapshot(2)) {
		t.Fatalf("saved settings differ: %+v vs %+v", loaded, c.Snapshot(2))
	}
}

// a write that hangs on a dead port must not block readers of the settings
func TestStuckWriteDoesNotBlockSnapshot(t *testing.T) {
	tr := &fakeTransport{connected: true, block: make(chan struct{})}
	c := newTestController(t, tr)

	go c.PushAll() // gets stuck inside Write

	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		c.Snapshot(2)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot blocked while a serial write was stuck")
	}

	close(tr.block) // let the stuck push finish
}
