package deej

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"go.bug.st/serial/enumerator"
)

const (
	maxEditorSliders    = 32
	maxTargetsPerSlider = 16
	maxTargetLength     = 128
)

// listKeys returns the keys of all audio sessions currently in the map
func (m *sessionMap) listKeys() []string {
	m.lock.Lock()
	defer m.lock.Unlock()

	keys := make([]string, 0, len(m.m))
	for key := range m.m {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	return keys
}

// SaveSliderMapping writes slider_mapping to the user's config file.
// It uses a separate viper instance on purpose: Set() on userConfig would stay as an override
// and later manual edits of config.yaml would be ignored. The file watcher reloads the config afterwards.
// NOTE: viper rewrites the whole file, so comments in config.yaml are lost.
func (cc *CanonicalConfig) SaveSliderMapping(mapping map[int][]string) error {
	v := viper.New()
	v.SetConfigFile(cc.userConfig.ConfigFileUsed())

	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	raw := make(map[string]interface{}, len(mapping))
	for idx, targets := range mapping {
		raw[strconv.Itoa(idx)] = targets
	}

	v.Set("slider_mapping", raw)

	if err := v.WriteConfig(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

func (s *ledServer) registerEditorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/ports", s.handlePorts)
	mux.HandleFunc("/api/mapping", s.handleMapping)
	mux.HandleFunc("/api/port", s.handlePort)
}

func (s *ledServer) editorReady(w http.ResponseWriter) bool {
	if s.deej == nil || s.deej.sessions == nil || s.deej.config == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "editor not available yet"})
		return false
	}

	return true
}

type ledSessionInfo struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // special | device | app
}

func (s *ledServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAPI(w, r, http.MethodGet) || !s.editorReady(w) {
		return
	}

	if r.URL.Query().Get("refresh") == "1" {
		s.deej.sessions.refreshSessions(true)
	}

	special := map[string]bool{masterSessionName: true, systemSessionName: true, inputSessionName: true}
	out := []ledSessionInfo{}

	for _, key := range s.deej.sessions.listKeys() {
		kind := "app"

		switch {
		case special[key]:
			kind = "special"
		case deviceSessionKeyPattern.MatchString(key):
			kind = "device"
		}

		out = append(out, ledSessionInfo{Key: key, Kind: kind})
	}

	s.writeJSON(w, http.StatusOK, out)
}

type ledPortInfo struct {
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Active bool   `json:"active"`
}

func (s *ledServer) handlePorts(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAPI(w, r, http.MethodGet) || !s.editorReady(w) {
		return
	}

	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not list COM ports"})
		return
	}

	configured := strings.ToLower(s.deej.config.ConnectionInfo.COMPort)
	out := []ledPortInfo{}

	for _, p := range ports {
		desc := p.Product
		if p.IsUSB {
			desc = strings.TrimSpace(fmt.Sprintf("%s (USB %s:%s)", p.Product, p.VID, p.PID))
		}

		out = append(out, ledPortInfo{
			Name:   p.Name,
			Desc:   desc,
			Active: strings.ToLower(p.Name) == configured,
		})
	}

	s.writeJSON(w, http.StatusOK, out)
}

type ledMapping struct {
	Sliders [][]string `json:"sliders"`
}

func (s *ledServer) handleMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.setMapping(w, r)
		return
	}

	if !s.authorizeAPI(w, r, http.MethodGet) || !s.editorReady(w) {
		return
	}

	n := s.ctl.sliderCount()
	res := ledMapping{Sliders: make([][]string, n)}

	for i := range res.Sliders {
		res.Sliders[i] = []string{}
	}

	s.deej.config.SliderMapping.iterate(func(idx int, targets []string) {
		if idx >= 0 && idx < n {
			res.Sliders[idx] = append([]string{}, targets...)
		}
	})

	s.writeJSON(w, http.StatusOK, res)
}

func (s *ledServer) setMapping(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAPI(w, r, http.MethodPost) || !s.editorReady(w) {
		return
	}

	var body ledMapping
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid data"})
		return
	}

	if len(body.Sliders) > maxEditorSliders {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many sliders"})
		return
	}

	mapping := map[int][]string{}

	for idx, targets := range body.Sliders {
		if len(targets) > maxTargetsPerSlider {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many targets for one slider"})
			return
		}

		seen := map[string]bool{}

		for _, t := range targets {
			t = strings.TrimSpace(t)

			if t == "" || len(t) > maxTargetLength || strings.ContainsAny(t, "\x00\r\n\t") {
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid target name"})
				return
			}

			if !seen[strings.ToLower(t)] {
				seen[strings.ToLower(t)] = true
				mapping[idx] = append(mapping[idx], t)
			}
		}
	}

	// keep mappings of sliders the page doesn't know about (index beyond what it sent)
	s.deej.config.SliderMapping.iterate(func(idx int, targets []string) {
		if idx >= len(body.Sliders) {
			mapping[idx] = append([]string{}, targets...)
		}
	})

	if err := s.deej.config.SaveSliderMapping(mapping); err != nil {
		s.logger.Warnw("Failed to save slider mapping", "error", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save config.yaml"})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// SaveCOMPort writes com_port to the user's config file (separate viper instance, see SaveSliderMapping)
func (cc *CanonicalConfig) SaveCOMPort(port string) error {
	v := viper.New()
	v.SetConfigFile(cc.userConfig.ConfigFileUsed())

	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	v.Set("com_port", port)

	if err := v.WriteConfig(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

func (s *ledServer) handlePort(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAPI(w, r, http.MethodPost) || !s.editorReady(w) {
		return
	}

	var body struct {
		Name string `json:"name"`
	}

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid data"})
		return
	}

	// only accept ports that actually exist right now
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not list COM ports"})
		return
	}

	found := ""
	for _, p := range ports {
		if p.Name == body.Name {
			found = p.Name
			break
		}
	}

	if found == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown COM port"})
		return
	}

	if err := s.deej.config.SaveCOMPort(found); err != nil {
		s.logger.Warnw("Failed to save COM port", "error", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save config.yaml"})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// AttachDeej gives the settings page access to sessions, config and COM ports
func (c *LEDController) AttachDeej(d *Deej) {
	c.server.deej = d
}
