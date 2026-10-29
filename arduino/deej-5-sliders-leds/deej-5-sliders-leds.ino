// deej with two-way serial: slider values go to the PC (as in the vanilla
// sketch), LED colors/gradients come back from the PC.
//
// Assumed hardware (adjust the constants below to match your build):
//   - one WS2812/NeoPixel strip per slider, each on its own pin (LED_PINS,
//     same order as the sliders), LEDS_PER_SLIDER LEDs per strip
//   - library: "Adafruit NeoPixel" (Library Manager)
//
// Protocol (ASCII, one command per line, '\n' terminated)
//   PC -> board
//     L<slider>:<RRGGBB>[,<RRGGBB>...]   1 to 4 color stops; 1 stop = solid color,
//                                        more = gradient spread over the slider's LEDs
//                                        e.g.  L2:ff0000,0000ff
//     B<0-255>                           global brightness
//     ?                                  ping, board answers with READY
//   board -> PC
//     v0|v1|v2|v3|v4                     slider values (unchanged from vanilla deej)
//     READY                              sent on boot and as answer to '?'
//
// LED behavior: each strip works as a level meter. LEDs light up from LED 0
// up to the slider position, the color of each LED comes from the slider's
// color/gradient, and the last lit LED fades in smoothly.
//
// Calibration: the sliders' raw readings aren't linear over their whole
// travel, so calibrate() maps them piecewise onto 0-1023 (see there). Both the
// LEDs and the values sent to deej (i.e. the volume) use the corrected value.
// To re-measure, set CALIBRATION_MODE to true, flash, close deej and open the
// Arduino serial monitor (9600 baud): you get the raw values plus a line every
// second with the min/max seen per slider.
//
// Note: boards with a hardware UART (Nano/Uno) can lose incoming bytes while
// the LEDs are being updated at 9600 baud. If you see glitches, raise the baud
// rate here AND in deej's config.yaml (e.g. 115200). Micro/Leonardo/Pro Micro
// use native USB and are not affected.

#include <Adafruit_NeoPixel.h>

const int NUM_SLIDERS = 5;
const int analogInputs[NUM_SLIDERS] = {A3, A2, A1, A0, 10};

const uint8_t LED_PINS[NUM_SLIDERS] = {4, 5, 6, 7, 8};
const uint8_t LEDS_PER_SLIDER = 10;
const uint8_t MAX_STOPS = 4;

const bool CALIBRATION_MODE = false;


// LEDs only follow the slider if it moved at least this much (of 1023), so
// analog noise doesn't make the last LED flicker
const int LED_DEADBAND = 4;

// default-constructed and configured in setup(): copying NeoPixel objects
// (e.g. via an initializer list) would free their pixel buffers twice
Adafruit_NeoPixel strips[NUM_SLIDERS];

int analogSliderValues[NUM_SLIDERS]; // calibrated, 0-1023
int ledLevelValues[NUM_SLIDERS];     // value the LEDs currently show (deadbanded)
int seenMin[NUM_SLIDERS];            // calibration mode only
int seenMax[NUM_SLIDERS];
unsigned long lastCalReport = 0;

// LED state is kept in "recipe" form (stops + brightness) and rendered on
// demand. We deliberately don't use setBrightness(): it scales the pixel
// buffer in place and is lossy when called repeatedly.
uint8_t stopCount[NUM_SLIDERS];
uint8_t stops[NUM_SLIDERS][MAX_STOPS][3];
uint8_t brightness = 64; // conservative until the PC sends its own value (USB power budget!)
bool sliderDirty[NUM_SLIDERS];

char rxBuf[64];
uint8_t rxLen = 0;
bool rxOverflow = false;

void setup() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
    pinMode(analogInputs[i], INPUT);
    stopCount[i] = 1; // black until the PC tells us otherwise
    seenMin[i] = 1023;
    seenMax[i] = 0;
  }

  for (int i = 0; i < NUM_SLIDERS; i++) {
    strips[i].updateType(NEO_GRB + NEO_KHZ800);
    strips[i].updateLength(LEDS_PER_SLIDER);
    strips[i].setPin(LED_PINS[i]);
    strips[i].begin();
    strips[i].show();
  }

  Serial.begin(9600);
  Serial.println("READY");
}

void loop() {
  readCommands();
  updateSliderValues();
  sendSliderValues();

  renderDirty();

  if (CALIBRATION_MODE) {
    reportCalibration();
  }

  delay(10);
}

// ---- slider values -> PC -------------------------------------------------

// clamp to the linear range and stretch it to 0-1023
// piecewise correction of the raw reading (0-1023) to a linear 0-1023:
//   raw   0..29   ->    0..100   (bottom end)
//   raw  30..900  ->  101..799   (linear middle)
//   raw 901..1023 ->  800..1023  (top end)
int calibrate(int raw) {
  long v;

  if (raw < 30) {
    v = map(raw, 0, 30, 0, 100);
  } else if (raw > 900) {
    v = map(raw, 900, 1023, 800, 1023);
  } else {
    v = map(raw, 31, 899, 101, 799);
  }

  return (int)constrain(v, 0, 1023);
}

void reportCalibration() {
  if (millis() - lastCalReport < 1000) return;
  lastCalReport = millis();

  Serial.print("CAL min-max:");
  for (int i = 0; i < NUM_SLIDERS; i++) {
    Serial.print(' ');
    Serial.print(seenMin[i]);
    Serial.print('-');
    Serial.print(seenMax[i]);
  }
  Serial.println();
}

void updateSliderValues() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
    int raw = analogRead(analogInputs[i]);
    if (raw < seenMin[i]) seenMin[i] = raw;
    if (raw > seenMax[i]) seenMax[i] = raw;

    // in calibration mode we pass the raw value through untouched
    int value = CALIBRATION_MODE ? raw : calibrate(raw);
    analogSliderValues[i] = value;

    // the ends always snap, in between the deadband applies
    bool moved = abs(value - ledLevelValues[i]) >= LED_DEADBAND ||
                 (value != ledLevelValues[i] && (value == 0 || value == 1023));
    if (moved) {
      ledLevelValues[i] = value;
      sliderDirty[i] = true;
    }
  }
}

void sendSliderValues() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
    Serial.print(analogSliderValues[i]);
    if (i < NUM_SLIDERS - 1) {
      Serial.print('|');
    }
  }
  Serial.println(); // "\r\n", which is what deej's line pattern expects
}

// ---- commands PC -> board ------------------------------------------------

void readCommands() {
  while (Serial.available() > 0) {
    char c = (char)Serial.read();

    if (c == '\r') {
      continue;
    }

    if (c == '\n') {
      if (!rxOverflow && rxLen > 0) {
        rxBuf[rxLen] = '\0';
        handleCommand(rxBuf);
      }
      rxLen = 0;
      rxOverflow = false;
      continue;
    }

    if (rxLen < sizeof(rxBuf) - 1) {
      rxBuf[rxLen++] = c;
    } else {
      rxOverflow = true; // drop the rest of this line
    }
  }
}

void handleCommand(const char *line) {
  switch (line[0]) {
    case '?':
      Serial.println("READY");
      break;

    case 'B': {
      int value = atoi(line + 1);
      brightness = (uint8_t)constrain(value, 0, 255);
      markAllDirty();
      break;
    }

    case 'L':
      parseSlider(line + 1);
      break;

    default:
      break; // unknown command, ignore
  }
}

int hexNibble(char c) {
  if (c >= '0' && c <= '9') return c - '0';
  if (c >= 'a' && c <= 'f') return c - 'a' + 10;
  if (c >= 'A' && c <= 'F') return c - 'A' + 10;
  return -1;
}

// expects "<slider>:<RRGGBB>[,<RRGGBB>...]"
void parseSlider(const char *p) {
  int slider = 0;
  if (*p < '0' || *p > '9') return;
  while (*p >= '0' && *p <= '9') {
    slider = slider * 10 + (*p - '0');
    p++;
    if (slider > 255) return;
  }
  if (*p != ':' || slider >= NUM_SLIDERS) return;
  p++;

  // parse into a scratch buffer first so a malformed line never leaves a
  // slider half-updated
  uint8_t tmp[MAX_STOPS][3];
  uint8_t count = 0;

  while (count < MAX_STOPS) {
    for (uint8_t ch = 0; ch < 3; ch++) {
      int hi = hexNibble(p[0]);
      int lo = (hi < 0) ? -1 : hexNibble(p[1]);
      if (hi < 0 || lo < 0) return;
      tmp[count][ch] = (uint8_t)(hi * 16 + lo);
      p += 2;
    }
    count++;

    if (*p == ',') {
      p++;
    } else {
      break;
    }
  }

  if (*p != '\0' || count == 0) return;

  stopCount[slider] = count;
  memcpy(stops[slider], tmp, sizeof(tmp));
  sliderDirty[slider] = true;
}

// ---- rendering -----------------------------------------------------------

void markAllDirty() {
  for (uint8_t s = 0; s < NUM_SLIDERS; s++) {
    sliderDirty[s] = true;
  }
}

void renderDirty() {
  for (uint8_t s = 0; s < NUM_SLIDERS; s++) {
    if (sliderDirty[s]) {
      renderSlider(s);
      strips[s].show();
      sliderDirty[s] = false;
    }
  }
}

void renderSlider(uint8_t s) {
  uint8_t n = stopCount[s];

  // fill level in 1/256 LED units: 0 = all off, LEDS_PER_SLIDER*256 = all on
  uint32_t level = (uint32_t)ledLevelValues[s] * LEDS_PER_SLIDER * 256UL / 1023;

  for (uint8_t j = 0; j < LEDS_PER_SLIDER; j++) {
    // how much of this LED is lit: full below the level, partial on the
    // boundary LED, off above
    uint32_t ledStart = (uint32_t)j * 256;
    uint8_t fill;
    if (level >= ledStart + 256) {
      fill = 255;
    } else if (level <= ledStart) {
      fill = 0;
    } else {
      fill = (uint8_t)(level - ledStart);
    }

    if (fill == 0) {
      strips[s].setPixelColor(j, 0);
      continue;
    }

    uint8_t rgb[3];

    if (n <= 1 || LEDS_PER_SLIDER < 2) {
      rgb[0] = stops[s][0][0];
      rgb[1] = stops[s][0][1];
      rgb[2] = stops[s][0][2];
    } else {
      // position along the whole gradient in 1/256 segment units
      uint32_t pos = (uint32_t)j * (n - 1) * 256UL / (LEDS_PER_SLIDER - 1);
      uint8_t seg = (uint8_t)(pos >> 8);
      uint8_t frac = (uint8_t)(pos & 0xFF);

      if (seg >= n - 1) { // last LED lands exactly on the last stop
        rgb[0] = stops[s][n - 1][0];
        rgb[1] = stops[s][n - 1][1];
        rgb[2] = stops[s][n - 1][2];
      } else {
        for (uint8_t ch = 0; ch < 3; ch++) {
          int16_t a = stops[s][seg][ch];
          int16_t b = stops[s][seg + 1][ch];
          rgb[ch] = (uint8_t)(a + ((b - a) * (int16_t)frac) / 256);
        }
      }
    }

    uint8_t scale = ((uint16_t)brightness * fill) / 255;
    uint8_t r = ((uint16_t)rgb[0] * scale) / 255;
    uint8_t g = ((uint16_t)rgb[1] * scale) / 255;
    uint8_t b = ((uint16_t)rgb[2] * scale) / 255;

    strips[s].setPixelColor(j, strips[s].Color(r, g, b));
  }
}
