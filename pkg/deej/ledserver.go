package deej

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed" // for the LED settings page
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

//go:embed ui/leds.html
var ledUIHTML string

const (
	ledTokenPlaceholder = "__DEEJ_TOKEN__"
	ledTokenHeader      = "X-Deej-Token"
)

// ledServer serves the LED settings page on a random localhost port.
//
// Since any website open in the user's browser can talk to localhost, access is
// restricted: the Host header must match (defeats DNS rebinding) and every
// request needs a random per-run token. The API wants it in a custom header,
// which cross-origin pages can't send without a CORS preflight we never answer
type ledServer struct {
	logger *zap.SugaredLogger
	ctl    *LEDController

	mu    sync.Mutex
	url   string
	token string
	port  int
}

func newLEDServer(logger *zap.SugaredLogger, ctl *LEDController) *ledServer {
	return &ledServer{
		logger: logger.Named("ui"),
		ctl:    ctl,
	}
}

// ensureStarted starts listening on first use and returns the URL to open
func (s *ledServer) ensureStarted() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.url != "" {
		return s.url, nil
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	s.token = hex.EncodeToString(tokenBytes)
	s.port = listener.Addr().(*net.TCPAddr).Port
	s.url = fmt.Sprintf("http://127.0.0.1:%d/?t=%s", s.port, s.token)

	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Warnw("LED settings server stopped", "error", err)
		}
	}()

	s.logger.Infow("LED settings server listening", "port", s.port)

	return s.url, nil
}

func (s *ledServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/leds", s.handleSet)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		mux.ServeHTTP(w, r)
	})
}

func (s *ledServer) hostAllowed(host string) bool {
	return host == fmt.Sprintf("127.0.0.1:%d", s.port) || host == fmt.Sprintf("localhost:%d", s.port)
}

func (s *ledServer) tokenValid(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

func (s *ledServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if !s.tokenValid(r.URL.Query().Get("t")) {
		http.Error(w, "Link ungültig - bitte über das deej-Tray-Menü erneut öffnen.", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")

	fmt.Fprint(w, strings.Replace(ledUIHTML, ledTokenPlaceholder, s.token, 1))
}

type ledStateResponse struct {
	Connected  bool        `json:"connected"`
	BoardReady bool        `json:"boardReady"`
	NumSliders int         `json:"numSliders"`
	Labels     []string    `json:"labels"`
	Settings   LEDSettings `json:"settings"`
}

func (s *ledServer) handleState(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAPI(w, r, http.MethodGet) {
		return
	}

	n := s.ctl.sliderCount()

	s.writeJSON(w, http.StatusOK, ledStateResponse{
		Connected:  s.ctl.transport.Connected(),
		BoardReady: s.ctl.transport.BoardReady(),
		NumSliders: n,
		Labels:     s.ctl.sliderLabels(n),
		Settings:   s.ctl.Snapshot(n),
	})
}

func (s *ledServer) handleSet(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAPI(w, r, http.MethodPost) {
		return
	}

	var settings LEDSettings

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := decoder.Decode(&settings); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ungültige Daten"})
		return
	}

	save := r.URL.Query().Get("save") == "1"

	if err := s.ctl.Apply(settings, save); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *ledServer) authorizeAPI(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}

	if !s.tokenValid(r.Header.Get(ledTokenHeader)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}

	return true
}

func (s *ledServer) writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Debugw("Failed to write JSON response", "error", err)
	}
}
