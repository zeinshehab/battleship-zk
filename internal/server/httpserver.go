package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"battleship-zk/internal/app"
	"battleship-zk/internal/codec"
	"battleship-zk/internal/game"
	"battleship-zk/internal/merkle"
	"battleship-zk/web"
)

const totalShipCells = 17

type Server struct {
	KeysDir    string
	SecretPath string          // kept for CLI compatibility; no longer used for I/O
	VKPath     string          // e.g., KeysDir + "/shot.vk"

	// In-memory state (no JSON persistence)
	mu        sync.RWMutex
	sec       *codec.Secret
	peer      *PeerInfo
	turn      *turnState
	game      *gameState
	lastEvt   *ShotEvent
	shotsTried map[string]bool

	// Milliseconds since epoch when THIS server booted (authoritative liveness marker)
	startAt int64
}

type PeerInfo struct {
	BaseURL string `json:"baseUrl"`          // e.g. http://192.168.1.55:8081
	RootHex string `json:"rootHex,omitempty"`
	VKB64   string `json:"vkB64,omitempty"`
}

func New(keysDir, secretPath string) *Server {
	s := &Server{
		KeysDir:     keysDir,
		SecretPath:  secretPath, // kept but unused for storage
		VKPath:      filepath.Join(keysDir, "shot.vk"),
		shotsTried:  make(map[string]bool),
		startAt:     time.Now().UnixMilli(),
		turn:        &turnState{MyTurn: "", Ready: false, Decided: false},
		game:        &gameState{},
	}
	return s
}

func (s *Server) Routes(mux *http.ServeMux) {
	// Actions you KEEP
	mux.HandleFunc("/v1/init", s.handleInit)
	mux.HandleFunc("/v1/commit", s.handleCommit)
	mux.HandleFunc("/v1/shoot", s.handleShoot)
	mux.HandleFunc("/v1/verify", s.handleVerify)

	// Consolidated READ
	mux.HandleFunc("/v1/status", s.handleStatus)

	// Tightened pairing/handshake (idempotent)
	mux.HandleFunc("/v1/peer", s.handlePeerPut) // expects PUT

	// Serve embedded GUI at /
	gui := http.FileServer(web.FS())
	mux.Handle("/", gui)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// === Secret / Root helpers ===

func (s *Server) currentSecret() (*codec.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sec != nil {
		return s.sec, nil
	}
	return nil, fmt.Errorf("no secret committed yet")
}

func computeRootHex(sec *codec.Secret) (string, error) {
	if sec.SaltHex == "" || len(sec.SaltHex) < 3 || sec.SaltHex[:2] != "0x" {
		return "", fmt.Errorf("missing or invalid salt in secret")
	}
	salt, ok := new(big.Int).SetString(sec.SaltHex[2:], 16)
	if !ok {
		return "", fmt.Errorf("cannot parse salt")
	}
	treeRoot := sec.Tree.Root()
	salted := merkle.HashNodeMiMC(salt, treeRoot)
	return fmt.Sprintf("0x%x", salted), nil
}

// === Init / Commit ===

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	b, err := app.InitBoard()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, b)
}

type commitReq struct {
	Board game.Board `json:"board"`
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req commitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	res, err := app.Commit(req.Board, s.KeysDir)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	// In-memory: store defender secret
	s.mu.Lock()
	s.sec = &res.Secret
	s.mu.Unlock()

	// Compute salted root and store in in-memory turn state
	rootHex, _ := computeRootHex(&res.Secret)
	_, _ = s.updateTurn(func(t *turnState) { t.MyRootHex = rootHex })

	writeJSON(w, 200, map[string]any{"rootHex": rootHex})
}

// === Shoot / Verify ===

type shootReq struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

func (s *Server) handleShoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req shootReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}

	// Turn gating: defender only accepts shot when opponent's turn (from our perspective)
	t, err := s.loadTurn()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to read turn state"})
		return
	}
	if !t.Ready || t.MyTurn != "opponent" {
		writeJSON(w, 409, map[string]any{
			"error":   "not allowed: it's not opponent's turn to shoot",
			"myTurn":  t.MyTurn,
			"ready":   t.Ready,
			"decided": t.Decided,
		})
		return
	}
	// Block if game is already over
	if g, gErr := s.loadGame(); gErr == nil && g.Over {
		writeJSON(w, 409, map[string]any{
			"error":     "game is over",
			"winner":    g.Winner,
			"hitsTaken": g.HitsTaken,
			"hitsDealt": g.HitsDealt,
		})
		return
	}

	// Duplicate-shot gating (reservation to avoid races)
	k := shotKey(req.Row, req.Col)
	s.mu.Lock()
	if s.shotsTried == nil {
		s.shotsTried = make(map[string]bool)
	}
	if s.shotsTried[k] {
		s.mu.Unlock()
		writeJSON(w, 409, map[string]any{
			"error":   "cell already targeted",
			"row":     req.Row,
			"col":     req.Col,
			"myTurn":  t.MyTurn,
			"ready":   t.Ready,
			"decided": t.Decided,
		})
		return
	}
	s.shotsTried[k] = true // reserve
	s.mu.Unlock()

	sec, err := s.currentSecret()
	if err != nil {
		// rollback reservation
		s.mu.Lock()
		delete(s.shotsTried, k)
		s.mu.Unlock()
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	res, err := app.Shoot(*sec, s.KeysDir, req.Row, req.Col)
	if err != nil {
		// rollback reservation on failure so attacker can try again if needed
		s.mu.Lock()
		delete(s.shotsTried, k)
		s.mu.Unlock()
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	// Defender: remember shot so UI can color own board
	s.recordShot(req.Row, req.Col, res.Bit)

	// Update defense-side game state on hit
	if res.Bit == 1 {
		_, _ = s.updateGame(func(g *gameState) {
			if !g.Over {
				g.HitsTaken++
				if g.HitsTaken >= totalShipCells {
					g.Over = true
					g.Winner = "opponent"
				}
			}
		})
	}

	// Include local VK as base64 (best effort)
	var vkB64 string
	if data, err := os.ReadFile(s.VKPath); err == nil && len(data) > 0 {
		vkB64 = base64.StdEncoding.EncodeToString(data)
	}

	// Compute rootHex now (defender's current salted root)
	rootHex, err := computeRootHex(sec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	// After receiving a valid shot, it's now our turn locally
	_, _ = s.updateTurn(func(t *turnState) { t.MyTurn = "me" })

	resp := map[string]any{
		"payload": res.Payload,
		"bit":     res.Bit,
		"rootHex": rootHex,
		"vkB64":   vkB64,
	}
	writeJSON(w, 200, resp)
}

type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

type verifyReq struct {
	RootHex string          `json:"rootHex,omitempty"`
	RootDec flexString      `json:"rootDec,omitempty"`
	Payload json.RawMessage `json:"payload"`
	VKB64   string          `json:"vkB64,omitempty"`
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req verifyReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
		return
	}

	// Attacker-side gating: only when it's our turn
	t, err := s.loadTurn()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to read turn state"})
		return
	}
	if !t.Ready || t.MyTurn != "me" {
		writeJSON(w, 409, map[string]any{
			"error":   "not allowed: it's not our turn to verify an attack",
			"myTurn":  t.MyTurn,
			"ready":   t.Ready,
			"decided": t.Decided,
		})
		return
	}
	if g, gErr := s.loadGame(); gErr == nil && g.Over {
		writeJSON(w, 409, map[string]any{
			"error":     "game is over",
			"winner":    g.Winner,
			"hitsTaken": g.HitsTaken,
			"hitsDealt": g.HitsDealt,
		})
		return
	}

	// VK required (defender's VK)
	if strings.TrimSpace(req.VKB64) == "" {
		writeJSON(w, 400, map[string]string{"error": "vkB64 required (use defender's VK)"})
		return
	}
	rawVK, err := base64.StdEncoding.DecodeString(req.VKB64)
	if err != nil || len(rawVK) == 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid vkB64"})
		return
	}
	// Temporary file only for verifier API that expects a path (not JSON; ephemeral)
	f, err := os.CreateTemp("", "vk-*.vk")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if _, err := f.Write(rawVK); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	_ = f.Close()
	vkPath := f.Name()
	defer os.Remove(vkPath)

	// Parse salted root (hex OR dec), but ignore any public.root in payload
	var rootInt *big.Int
	if strings.TrimSpace(req.RootHex) != "" {
		h := req.RootHex
		if !strings.HasPrefix(h, "0x") && !strings.HasPrefix(h, "0X") {
			h = "0x" + h
		}
		n := new(big.Int)
		if _, ok := n.SetString(h[2:], 16); !ok {
			writeJSON(w, 400, map[string]string{"error": "invalid rootHex"})
			return
		}
		rootInt = n
	} else if strings.TrimSpace(string(req.RootDec)) != "" {
		n := new(big.Int)
		if _, ok := n.SetString(string(req.RootDec), 10); !ok {
			writeJSON(w, 400, map[string]string{"error": "invalid rootDec"})
			return
		}
		rootInt = n
	} else {
		writeJSON(w, 400, map[string]string{"error": "must provide rootHex or rootDec"})
		return
	}

	var payloadMap map[string]any
	if err := json.Unmarshal(req.Payload, &payloadMap); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json in payload"})
		return
	}
	if pub, ok := payloadMap["public"].(map[string]any); ok {
		delete(pub, "root")
	}
	payloadSanitized, _ := json.Marshal(payloadMap)

	var payload codec.ShotProofPayload
	if err := json.Unmarshal(payloadSanitized, &payload); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json in payload: " + err.Error()})
		return
	}

	res, err := app.VerifyWithRoot(vkPath, rootInt, payload)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	// If verification succeeded, it's now opponent's turn locally
	if res.Valid {
		_, _ = s.updateTurn(func(t *turnState) { t.MyTurn = "opponent" })
	}

	// Attack-side game state update on hit
	if res.Hit == 1 {
		_, _ = s.updateGame(func(g *gameState) {
			if !g.Over {
				g.HitsDealt++
				if g.HitsDealt >= totalShipCells {
					g.Over = true
					g.Winner = "me"
				}
			}
		})
	}

	writeJSON(w, 200, res)
}

// === Consolidated STATUS ===

func (s *Server) loadVKB64() string {
	data, err := os.ReadFile(s.VKPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

func (s *Server) statusPayload() map[string]any {
	s.mu.RLock()
	t := *s.turn
	g := *s.game
	ev := s.lastEvt
	peer := s.peer
	s.mu.RUnlock()

	defense := any(map[string]any{"n": 0})
	if ev != nil {
		defense = map[string]any{
			"row": ev.Row, "col": ev.Col, "bit": ev.Bit, "n": ev.N, "at": ev.At,
		}
	}

	return map[string]any{
		"startedAt": s.startAt,
		"myId":      t.MyID,
		"oppId":     t.OppID,

		"myRootHex":  t.MyRootHex,
		"oppRootHex": t.OppRootHex,

		"peer": peer, // snapshot of last-set peer (may be nil)

		"turn": map[string]any{
			"myTurn":  t.MyTurn,
			"ready":   t.Ready,
			"decided": t.Decided,
		},
		"game": map[string]any{
			"hitsTaken": g.HitsTaken,
			"hitsDealt": g.HitsDealt,
			"over":      g.Over,
			"winner":    g.Winner,
		},
		"vkB64":       s.loadVKB64(),
		"defenseLast": defense,
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, 200, s.statusPayload())
}

// === Pairing / Handshake (PUT /v1/peer) ===

func selfBaseURL(r *http.Request) string {
	scheme := "http"
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		scheme = xfp
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Host)
	return scheme + "://" + strings.TrimRight(host, "/")
}

type peerPutReq struct {
	BaseURL string `json:"baseUrl"`           // required
	RootHex string `json:"rootHex,omitempty"` // optional: opponent salted root
	VKB64   string `json:"vkB64,omitempty"`   // optional: opponent VK (if shared OOB)
}

func (s *Server) handlePeerPut(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req peerPutReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.BaseURL) == "" {
		writeJSON(w, 400, map[string]string{"error": "bad json or missing baseUrl"})
		return
	}

	// In-memory peer snapshot
	s.mu.Lock()
	s.peer = &PeerInfo{
		BaseURL: strings.TrimRight(req.BaseURL, "/"),
		RootHex: req.RootHex,
		VKB64:   req.VKB64,
	}
	s.mu.Unlock()

	// Update turn state: ensure MyID set (if empty), set OppID and (optionally) OppRootHex
	_, _ = s.updateTurn(func(t *turnState) {
		if strings.TrimSpace(t.MyID) == "" {
			t.MyID = selfBaseURL(r)
		}
		t.OppID = strings.TrimRight(req.BaseURL, "/")
		if strings.TrimSpace(req.RootHex) != "" {
			t.OppRootHex = req.RootHex
		}
	})

	// Return unified status
	writeJSON(w, 200, s.statusPayload())
}

// === CORS ===

func WithCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In dev we allow any origin. For production, set this to the specific origin(s).
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// === Turn state & decision (in-memory) ===

type turnState struct {
	MyTurn     string `json:"myTurn"` // "me" | "opponent" | ""
	MyRootHex  string `json:"myRootHex,omitempty"`
	OppRootHex string `json:"oppRootHex,omitempty"`
	Ready      bool   `json:"ready"`
	Decided    bool   `json:"decided"`
	MyID       string `json:"myId,omitempty"`
	OppID      string `json:"oppId,omitempty"`
}

func (s *Server) loadTurn() (*turnState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.turn == nil {
		return &turnState{}, nil
	}
	cp := *s.turn
	return &cp, nil
}

func normalizeID(sid string) string {
	sid = strings.TrimSpace(sid)
	sid = strings.TrimRight(sid, "/")
	return strings.ToLower(sid)
}

// Liveness via /v1/status (always available)
func (s *Server) ping(baseURL string) bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/v1/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Reads /v1/status from peer and extracts startedAt
func (s *Server) peerStatus(baseURL string) (online bool, startedAt int64) {
	if strings.TrimSpace(baseURL) == "" {
		return false, 0
	}
	url := strings.TrimRight(baseURL, "/") + "/v1/status"
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, 0
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return false, 0
	}
	if v, ok := m["startedAt"].(float64); ok && int64(v) > 0 {
		return true, int64(v)
	}
	return false, 0
}

// Decide exactly once using server start timestamps (tie-break by ID if equal)
// After Decided=true, we never change MyTurn again; we only refresh Ready.
func (s *Server) updateTurn(mut func(*turnState)) (*turnState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		s.turn = &turnState{}
	}
	// Apply mutation (may set MyID/OppID/roots)
	mut(s.turn)

	myID := normalizeID(s.turn.MyID)
	oppID := normalizeID(s.turn.OppID)
	haveIDs := myID != "" && oppID != ""

	online, oppStarted := false, int64(0)
	if haveIDs {
		online, oppStarted = s.peerStatus(oppID)
	}

	// If already decided, never change who starts; just refresh connectivity
	if s.turn.Decided {
		s.turn.Ready = haveIDs && online
		cp := *s.turn
		return &cp, nil
	}

	// Decide exactly once when BOTH have valid start timestamps
	myStarted := s.startAt
	if haveIDs && online && myStarted > 0 && oppStarted > 0 {
		var iStart bool
		if myStarted != oppStarted {
			iStart = myStarted < oppStarted // earlier server starts
		} else {
			// Millisecond tie — break deterministically by normalized IDs
			iStart = myID < oppID
		}
		if iStart {
			s.turn.MyTurn = "me"
		} else {
			s.turn.MyTurn = "opponent"
		}
		s.turn.Ready = true
		s.turn.Decided = true
	} else {
		// Not ready to decide yet
		s.turn.Ready = false
		s.turn.Decided = false
	}

	cp := *s.turn
	return &cp, nil
}

// === Defense last-shot (in-memory) ===

type ShotEvent struct {
	Row int   `json:"row"`
	Col int   `json:"col"`
	Bit uint8 `json:"bit"` // 0 miss, 1 hit
	N   int   `json:"n"`   // monotonic counter
	At  int64 `json:"at"`  // unix ms
}

func (s *Server) recordShot(row, col int, bit uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 1
	if s.lastEvt != nil {
		n = s.lastEvt.N + 1
	}
	s.lastEvt = &ShotEvent{
		Row: row, Col: col, Bit: bit, N: n,
		At: time.Now().UnixMilli(),
	}
}

// === Game state (in-memory) ===

type gameState struct {
	HitsTaken int    `json:"hitsTaken"` // opponent hit my ships (defense)
	HitsDealt int    `json:"hitsDealt"` // I hit opponent ships (attack verify)
	Over      bool   `json:"over"`
	Winner    string `json:"winner"` // "me" | "opponent" | ""
}

func (s *Server) loadGame() (*gameState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.game == nil {
		return &gameState{}, nil
	}
	cp := *s.game
	return &cp, nil
}

func (s *Server) updateGame(mut func(*gameState)) (*gameState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.game == nil {
		s.game = &gameState{}
	}
	mut(s.game)
	cp := *s.game
	return &cp, nil
}

// === Misc helpers ===

func shotKey(r, c int) string { return fmt.Sprintf("%d,%d", r, c) }
