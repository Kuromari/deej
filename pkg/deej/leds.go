package deej

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

const (
	ledsFilepath = "leds.json"

	ledMaxStops   = 4  // must match MAX_STOPS in the arduino sketch
	ledMaxSliders = 32 // sanity limit for incoming settings

	// deliberately conservative: a full chain of WS2812s at full white can draw
	// far more than a USB port delivers
	ledDefaultBrightness = 96
)

var defaultLEDColors = []string{"#ff3b30", "#ff9500", "#34c759", "#0a84ff", "#bf5af2"}

var ledHexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// LEDSlider describes the color(s) of a single slider's LEDs. One stop is a
// solid color, several stops are spread out as a gradient over the LEDs
type LEDSlider struct {
	Stops []string `json:"stops"`
}

// LEDSettings is everything the user can configure about the LEDs
type LEDSettings struct {
	Brightness int         `json:"brightness"` // 0-255
	Sliders    []LEDSlider `json:"sliders"`
}

// ledTransport is the part of SerialIO that the LED controller needs
type ledTransport interface {
	Write(data string) error
	Connected() bool
	BoardReady() bool
}

// LEDController owns the LED settings, persists them and pushes them to the board
type LEDController struct {
	logger       *zap.SugaredLogger
	transport    ledTransport
	filepath     string
	sliderCount  func() int
	sliderLabels func(n int) []string

	// pushMu serializes pushes, so lines reach the board in order. It is held while
	// writing to the serial port, which is why mu (below) must never be held for that.
	// lock order: pushMu -> mu
	pushMu sync.Mutex

	// mu guards settings, sentBrightness and sentLines
	mu             sync.Mutex
	settings       LEDSettings
	sentBrightness int            // -1 = unknown
	sentLines      map[int]string // last line successfully sent per slider

	server *ledServer
}

// NewLEDController creates a controller and loads previously saved settings, if any
func NewLEDController(logger *zap.SugaredLogger,
	transport ledTransport,
	sliderCount func() int,
	sliderLabels func(n int) []string) *LEDController {

	logger = logger.Named("leds")

	c := &LEDController{
		logger:         logger,
		transport:      transport,
		filepath:       ledsFilepath,
		sliderCount:    sliderCount,
		sliderLabels:   sliderLabels,
		sentBrightness: -1,
		sentLines:      map[int]string{},
	}

	c.settings = c.loadFromDisk()
	c.server = newLEDServer(logger, c)

	return c
}

// OpenUI starts the local settings server (if needed) and opens it in the browser
func (c *LEDController) OpenUI() error {
	url, err := c.server.ensureStarted()
	if err != nil {
		return fmt.Errorf("start led ui server: %w", err)
	}

	return util.OpenBrowser(c.logger, url)
}

// Apply validates the given settings, pushes whatever changed to the board and
// optionally persists them. Without save it acts as a live preview
func (c *LEDController) Apply(in LEDSettings, save bool) error {
	clean, err := sanitizeLEDSettings(in)
	if err != nil {
		return err
	}

	if err := c.update(clean, save); err != nil {
		return err
	}

	c.push(false)

	return nil
}

// update stores the new settings (and optionally persists them)
func (c *LEDController) update(clean LEDSettings, save bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// if the caller knows fewer sliders than we have stored, keep the rest
	if len(c.settings.Sliders) > len(clean.Sliders) {
		clean.Sliders = append(clean.Sliders, c.settings.Sliders[len(clean.Sliders):]...)
	}

	c.settings = clean

	if save {
		if err := c.saveToDisk(); err != nil {
			c.logger.Warnw("Failed to save LED settings", "error", err)
			return fmt.Errorf("failed to save LED settings: %w", err)
		}
	}

	return nil
}

// Snapshot returns a copy of the current settings, padded to at least n sliders
func (c *LEDController) Snapshot(n int) LEDSettings {
	c.mu.Lock()
	defer c.mu.Unlock()

	return padLEDSettings(copyLEDSettings(c.settings), n)
}

// PushAll re-sends the complete state to the board. Called when the board
// announces that it's ready (boot, reconnect)
func (c *LEDController) PushAll() {
	c.logger.Debug("Pushing full LED state to board")
	c.push(true)
}

type pendingLEDLine struct {
	sliderIdx int
	line      string
}

// push sends whatever the board doesn't know yet. The serial writes happen without
// holding c.mu, so a stuck port can't block Snapshot (and with it the settings page)
func (c *LEDController) push(force bool) {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()

	if !c.transport.Connected() {
		return
	}

	// decide what needs sending
	c.mu.Lock()

	if force {
		c.sentBrightness = -1
		c.sentLines = map[int]string{}
	}

	brightness := c.settings.Brightness
	sendBrightness := c.sentBrightness != brightness

	// send everything we have, the board ignores slider indexes it doesn't know.
	// this keeps us independent of whether the slider count has been detected yet
	var pending []pendingLEDLine
	for i := range c.settings.Sliders {
		line := ledLine(i, c.settings.Sliders[i])

		if c.sentLines[i] != line {
			pending = append(pending, pendingLEDLine{sliderIdx: i, line: line})
		}
	}

	c.mu.Unlock()

	// send it, and remember what made it to the board
	if sendBrightness {
		if err := c.transport.Write(fmt.Sprintf("B%d\n", brightness)); err != nil {
			c.logger.Debugw("Failed to send brightness", "error", err)
			return
		}

		c.mu.Lock()
		c.sentBrightness = brightness
		c.mu.Unlock()
	}

	for _, p := range pending {
		if err := c.transport.Write(p.line + "\n"); err != nil {
			c.logger.Debugw("Failed to send LED line", "slider", p.sliderIdx, "error", err)
			return
		}

		c.mu.Lock()
		c.sentLines[p.sliderIdx] = p.line
		c.mu.Unlock()
	}
}

func ledLine(sliderIdx int, s LEDSlider) string {
	hexes := make([]string, len(s.Stops))
	for i, stop := range s.Stops {
		hexes[i] = strings.TrimPrefix(stop, "#")
	}

	return fmt.Sprintf("L%d:%s", sliderIdx, strings.Join(hexes, ","))
}

func (c *LEDController) loadFromDisk() LEDSettings {
	defaults := padLEDSettings(LEDSettings{Brightness: ledDefaultBrightness}, len(defaultLEDColors))

	raw, err := os.ReadFile(c.filepath)
	if err != nil {
		if !os.IsNotExist(err) {
			c.logger.Warnw("Failed to read LED settings, using defaults", "error", err)
		}

		return defaults
	}

	var loaded LEDSettings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		c.logger.Warnw("LED settings file is not valid JSON, using defaults", "error", err)
		return defaults
	}

	clean, err := sanitizeLEDSettings(loaded)
	if err != nil {
		c.logger.Warnw("LED settings file has invalid contents, using defaults", "error", err)
		return defaults
	}

	c.logger.Infow("Loaded LED settings", "path", c.filepath, "sliders", len(clean.Sliders))

	return clean
}

// must be called with c.mu held
func (c *LEDController) saveToDisk() error {
	raw, err := json.MarshalIndent(c.settings, "", "  ")
	if err != nil {
		return err
	}

	// write-then-rename so a crash can't leave a half-written file behind
	tmp := c.filepath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return err
	}

	return os.Rename(tmp, c.filepath)
}

func sanitizeLEDSettings(in LEDSettings) (LEDSettings, error) {
	if in.Brightness < 0 || in.Brightness > 255 {
		return LEDSettings{}, errors.New("brightness must be between 0 and 255")
	}

	if len(in.Sliders) == 0 || len(in.Sliders) > ledMaxSliders {
		return LEDSettings{}, fmt.Errorf("number of sliders must be between 1 and %d", ledMaxSliders)
	}

	out := LEDSettings{
		Brightness: in.Brightness,
		Sliders:    make([]LEDSlider, len(in.Sliders)),
	}

	for i, slider := range in.Sliders {
		if len(slider.Stops) < 1 || len(slider.Stops) > ledMaxStops {
			return LEDSettings{}, fmt.Errorf("slider %d: expected 1 to %d colors", i+1, ledMaxStops)
		}

		out.Sliders[i].Stops = make([]string, len(slider.Stops))

		for j, stop := range slider.Stops {
			if !ledHexColorPattern.MatchString(stop) {
				return LEDSettings{}, fmt.Errorf("slider %d: invalid color %q", i+1, stop)
			}

			out.Sliders[i].Stops[j] = strings.ToLower(stop)
		}
	}

	return out, nil
}

func copyLEDSettings(in LEDSettings) LEDSettings {
	out := LEDSettings{Brightness: in.Brightness, Sliders: make([]LEDSlider, len(in.Sliders))}

	for i, s := range in.Sliders {
		out.Sliders[i].Stops = append([]string(nil), s.Stops...)
	}

	return out
}

func padLEDSettings(s LEDSettings, n int) LEDSettings {
	for len(s.Sliders) < n {
		color := defaultLEDColors[len(s.Sliders)%len(defaultLEDColors)]
		s.Sliders = append(s.Sliders, LEDSlider{Stops: []string{color}})
	}

	return s
}
