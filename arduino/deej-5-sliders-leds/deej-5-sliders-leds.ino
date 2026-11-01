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
//     H                                  heartbeat, board answers with HB. deej sends it
//                                        every few seconds and reconnects if the answer
//                                        stays away
//   board -> PC
//     v0|v1|v2|v3|v4                     slider values (unchanged from vanilla deej)
//     READY                              sent on boot and as answer to '?'
//     HB                                 answer to 'H'
//
// LED behavior: each strip works as a level meter. LEDs light up from LED 0
// up to the slider position, the color of each LED comes from the slider's
// color/gradient, and the last lit LED fades in smoothly.
//
// Linearization: the sliders' raw readings aren't linear over their whole
// travel, so linearize() maps them in three segments (see calStart/calEnd).
// Both the LEDs and the values sent to deej (i.e. the volume) use the
// corrected value.
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

// ---- linearization ----
// {raw value, target value}
//   raw <= calStart[0]         -> 0 .. calStart[1]
//   calStart[0] .. calEnd[0]   -> calStart[1] .. calEnd[1]
//   raw >= calEnd[0]           -> calEnd[1] .. 1024 (raw 1020 = end)
const int calStart[2] = {20, 180};
const int calEnd[2]   = {950, 940};

// LEDs only follow the slider if it moved at least this much (of 1023), so
// analog noise doesn't make the last LED flicker
const int LED_DEADBAND = 1;

// every slider keeps its last SMOOTH_SAMPLES raw readings and reports the
// highest of them, which suppresses single downward spikes (flicker)
const uint8_t SMOOTH_SAMPLES = 15;

// default-constructed and configured in setup(): copying NeoPixel objects
// (e.g. via an initializer list) would free their pixel buffers twice
Adafruit_NeoPixel strips[NUM_SLIDERS];

int analogSliderValues[NUM_SLIDERS]; // linearized, 0-1023
int ledLevelValues[NUM_SLIDERS];     // value the LEDs currently show (deadbanded)
int rawHistory[NUM_SLIDERS][SMOOTH_SAMPLES]; // ring buffer of raw readings
uint8_t historyPos = 0;
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

    // prefill the history so the first values aren't dragged down by zeros
    int first = analogRead(analogInputs[i]);
    for (uint8_t k = 0; k < SMOOTH_SAMPLES; k++) {
      rawHistory[i][k] = first;
    }
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

// three-segment correction of the raw reading to a linear 0-1023
int linearize(int raw) {
  long v;

  if (raw <= calStart[0]) {
    v = map(raw, 0, calStart[0], 0, calStart[1]);
  } else if (raw >= calEnd[0]) {
    v = map(raw, calEnd[0], 1020, calEnd[1], 1024);
  } else {
    v = map(raw, calStart[0], calEnd[0], calStart[1], calEnd[1]);
  }

  return (int)constrain(v, 0, 1024);
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
    int reading = analogRead(analogInputs[i]);
    if (reading < seenMin[i]) seenMin[i] = reading;
    if (reading > seenMax[i]) seenMax[i] = reading;

    // store the reading and take the highest of the last SMOOTH_SAMPLES
    rawHistory[i][historyPos] = reading;
    int raw = rawHistory[i][0];
    for (uint8_t k = 1; k < SMOOTH_SAMPLES; k++) {
      if (rawHistory[i][k] > raw) raw = rawHistory[i][k];
    }

    // in calibration mode we pass the (smoothed) raw value through uncorrected
    int value = CALIBRATION_MODE ? raw : linearize(raw);
    analogSliderValues[i] = value;

    // the ends always snap, in between the deadband applies
    bool moved = abs(value - ledLevelValues[i]) >= LED_DEADBAND ||
                 (value != ledLevelValues[i] && (value == 0 || value == 1023));
    if (moved) {
      ledLevelValues[i] = value;
      sliderDirty[i] = true;
    }
  }

  historyPos = (historyPos + 1) % SMOOTH_SAMPLES;
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

    case 'H':
      Serial.println("HB");
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

  // wie im alten Script: (int)(LEDS / 1022 * value), also abgerundet
  uint32_t lit = (uint32_t)ledLevelValues[s] * LEDS_PER_SLIDER / 1022;
  if (lit > LEDS_PER_SLIDER) lit = LEDS_PER_SLIDER;

  for (uint8_t j = 0; j < LEDS_PER_SLIDER; j++) {
    uint8_t fill = (j < lit) ? 255 : 0;

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