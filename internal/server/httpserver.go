package server

import (
	"bytes"
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
	SecretPath string
	VKPath     string // e.g., KeysDir + "/shot.vk"
	shotsTried map[string]bool 

	TurnPath string // persisted turn state JSON next to secret
	PeerPath string // persisted peer info JSON next to secret

	ShotsPath string  

	mu   sync.RWMutex
	sec  *codec.Secret
	peer *PeerInfo

	startAt int64 // milliseconds since epoch; when THIS server booted


	GamePath string // New

	lastEvt *ShotEvent 
}

type PeerInfo struct {
	BaseURL string `json:"baseUrl"`          // e.g. http://192.168.1.55:8081
	RootHex string `json:"rootHex,omitempty"`
	VKB64   string `json:"vkB64,omitempty"`
}

func New(keysDir, secretPath string) *Server {
	s := &Server{
		KeysDir:    keysDir,
		SecretPath: secretPath,
		VKPath:     filepath.Join(keysDir, "shot.vk"),
		shotsTried: make(map[string]bool),
		startAt:    time.Now().UnixMilli(), // NEW

	}
	dir := filepath.Dir(secretPath)
	s.TurnPath = filepath.Join(dir, "turn.json")
	s.PeerPath = filepath.Join(dir, "peer.json")
	s.ShotsPath = filepath.Join(dir, "last_shot.json") 
	s.GamePath = filepath.Join(dir, "game.json")
	
	_ = s.ensureTurn()
	_ = s.loadPeer()
	_ = s.ensureGame()
	if ev, err := s.loadLastShot(); err == nil { s.lastEvt = ev } 
	return s
}

func (s *Server) Routes(mux *http.ServeMux) {
	// API
	mux.HandleFunc("/v1/info", s.handleInfo)     // computed rootHex (no info.json)
	mux.HandleFunc("/v1/init", s.handleInit)
	mux.HandleFunc("/v1/commit", s.handleCommit)
	mux.HandleFunc("/v1/shoot", s.handleShoot)
	mux.HandleFunc("/v1/verify", s.handleVerify)

	// Peer mgmt
	mux.HandleFunc("/v1/peer", s.handlePeer)          // GET/POST
	mux.HandleFunc("/v1/send-info", s.handleSendInfo) // POST {toBaseUrl}

	mux.HandleFunc("/v1/turn", s.handleTurnGet)                 // GET
	mux.HandleFunc("/v1/turn/self", s.handleTurnSetSelf)        // POST {baseUrl}
	mux.HandleFunc("/v1/turn/opponent", s.handleTurnSetOppRoot) // POST {rootHex?, baseUrl?}
	mux.HandleFunc("/v1/turn/next", s.handleTurnNext)           // POST

	mux.HandleFunc("/v1/defense/last", s.handleDefenseLast) // GET: last incoming shot

	mux.HandleFunc("/v1/game/state", s.handleGameState) // GET
	mux.HandleFunc("/v1/game/reset", s.handleGameReset) // POST


	// Serve embedded GUI at /
	gui := http.FileServer(web.FS())
	mux.Handle("/", gui)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// --- Helpers to compute rootHex on the fly (no info.json) ---

func (s *Server) currentSecret() (*codec.Secret, error) {
	s.mu.RLock()
	sec := s.sec
	s.mu.RUnlock()
	if sec != nil { return sec, nil }

	f, err := os.Open(s.SecretPath)
	if err != nil { return nil, fmt.Errorf("no secret committed yet") }
	defer f.Close()
	var loaded codec.Secret
	if err := json.NewDecoder(f).Decode(&loaded); err != nil {
		return nil, fmt.Errorf("failed to read secret: %w", err)
	}
	s.mu.Lock()
	s.sec = &loaded
	s.mu.Unlock()
	return &loaded, nil
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

// --- /v1/info (dynamic) ---

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	sec, err := s.currentSecret()
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	rootHex, err := computeRootHex(sec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	// include vk bytes as base64 (best effort)
	var vkB64 string
	if data, err := os.ReadFile(s.VKPath); err == nil && len(data) > 0 {
		vkB64 = base64.StdEncoding.EncodeToString(data)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rootHex": rootHex,
		"vkB64":   vkB64,
	})
}

// --- Init / Commit ---

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
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
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
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

	// Persist defender secret to disk
	f, err := os.Create(s.SecretPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(f).Encode(res.Secret)
	_ = f.Close()

	// Update in-memory; mark Ready and store MyRootHex in turn.json
	s.mu.Lock()
	s.sec = &res.Secret
	s.mu.Unlock()

	rootHex, _ := computeRootHex(&res.Secret) // your helper—returns "0x..."
	_, _ = s.updateTurn(func(t *turnState){ t.MyRootHex = rootHex })

	writeJSON(w, 200, map[string]any{"rootHex": rootHex})
}

// --- Shoot / Verify (same working flow you have) ---

type shootReq struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

func (s *Server) handleShoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req shootReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}

	t, err := s.loadTurn()
    if err != nil {
        writeJSON(w, 500, map[string]string{"error": "failed to load turn state"})
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
    // Optional: block if game already over
    if g, gErr := s.loadGame(); gErr == nil && g.Over {
        writeJSON(w, 409, map[string]any{
            "error":     "game is over",
            "winner":    g.Winner,
            "hitsTaken": g.HitsTaken,
            "hitsDealt": g.HitsDealt,
        })
        return
    }

	// --- Duplicate-shot gating (with reservation to avoid races) ---
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
    // Reserve this cell to close the race window while we compute proof
    s.shotsTried[k] = true
    s.mu.Unlock()


	sec, err := s.currentSecret()
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	res, err := app.Shoot(*sec, s.KeysDir, req.Row, req.Col)
	// defender received a shot; remember it so UI can color own board

	if err != nil {
        // Roll back reservation on failure so attacker can retry a valid request later
        s.mu.Lock()
        delete(s.shotsTried, k)
        s.mu.Unlock()

        writeJSON(w, 400, map[string]string{"error": err.Error()})
        return
    }
	s.recordShot(req.Row, req.Col, res.Bit) 

	// New
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


	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	// Include vk bytes as base64 (best effort)
	var vkB64 string
	if data, err := os.ReadFile(s.VKPath); err == nil && len(data) > 0 {
		vkB64 = base64.StdEncoding.EncodeToString(data)
	}

	// Compute rootHex now
	rootHex, err := computeRootHex(sec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	_, _ = s.updateTurn(func(t *turnState){ t.MyTurn = "me" })


	resp := map[string]any{
		"payload": res.Payload,
		"bit":     res.Bit,
		"rootHex": rootHex,
		"vkB64":   vkB64,
	}
	writeJSON(w, 200, resp)
}

/*** --- verify: same robust version that injects trusted root and requires defender VK --- ***/
type flexString string
func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil { return err }
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil { return err }
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
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
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

	// === turn gating for attacker-side verify ===
    t, err := s.loadTurn()
    if err != nil {
        writeJSON(w, 500, map[string]string{"error": "failed to load turn state"})
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
    // ======

	if strings.TrimSpace(req.VKB64) == "" {
		writeJSON(w, 400, map[string]string{"error": "vkB64 required (use defender's VK)"}); return
	}
	rawVK, err := base64.StdEncoding.DecodeString(req.VKB64)
	if err != nil || len(rawVK) == 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid vkB64"}); return
	}
	f, err := os.CreateTemp("", "vk-*.vk")
	if err != nil { writeJSON(w, 500, map[string]string{"error": err.Error()}); return }
	if _, err := f.Write(rawVK); err != nil {
		_ = f.Close(); _ = os.Remove(f.Name())
		writeJSON(w, 500, map[string]string{"error": err.Error()}); return
	}
	_ = f.Close()
	vkPath := f.Name()
	defer os.Remove(vkPath)

	// Parse salted root
	var rootInt *big.Int
	if strings.TrimSpace(req.RootHex) != "" {
		h := req.RootHex
		if !strings.HasPrefix(h, "0x") && !strings.HasPrefix(h, "0X") { h = "0x" + h }
		n := new(big.Int)
		if _, ok := n.SetString(h[2:], 16); !ok { writeJSON(w, 400, map[string]string{"error": "invalid rootHex"}); return }
		rootInt = n
	} else if strings.TrimSpace(string(req.RootDec)) != "" {
		n := new(big.Int)
		if _, ok := n.SetString(string(req.RootDec), 10); !ok { writeJSON(w, 400, map[string]string{"error": "invalid rootDec"}); return }
		rootInt = n
	} else {
		writeJSON(w, 400, map[string]string{"error": "must provide rootHex or rootDec"}); return
	}

	// Sanitize payload: drop public.root (sci-notation issues)
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
		_, _ = s.updateTurn(func(t *turnState){ t.MyTurn = "opponent" })
	}

	// new
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

// --- Peer Management (unchanged) ---

func (s *Server) handlePeer(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.peer == nil {
			writeJSON(w, 200, map[string]any{"peer": nil})
			return
		}
		writeJSON(w, 200, s.peer)
	case http.MethodPost:
		var p PeerInfo
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if p.BaseURL == "" {
			writeJSON(w, 400, map[string]string{"error": "baseUrl required"})
			return
		}
		s.mu.Lock()
		s.peer = &p
		s.mu.Unlock()
		if err := s.savePeer(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type sendInfoReq struct {
	ToBaseURL string `json:"toBaseUrl"`
}

func (s *Server) handleSendInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req sendInfoReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ToBaseURL) == "" {
		writeJSON(w, 400, map[string]string{"error": "bad json or missing toBaseUrl"})
		return
	}

	// Prepare our info payload (rootHex + vkB64)
	sec, err := s.currentSecret()
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	rootHex, err := computeRootHex(sec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var vkB64 string
	if data, err := os.ReadFile(s.VKPath); err == nil && len(data) > 0 {
		vkB64 = base64.StdEncoding.EncodeToString(data)
	}
	payload := PeerInfo{ BaseURL: "", RootHex: rootHex, VKB64: vkB64 }
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Post(strings.TrimRight(req.ToBaseURL, "/")+"/v1/peer", "application/json", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "opponent offline or unreachable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "opponent returned non-200", "status": resp.StatusCode})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
// --- Peer persistence (unchanged) ---

func (s *Server) savePeer() error {
	if s.peer == nil { return nil }
	f, err := os.Create(s.PeerPath)
	if err != nil { return err }
	defer f.Close()
	return json.NewEncoder(f).Encode(s.peer)
}

func (s *Server) loadPeer() error {
	f, err := os.Open(s.PeerPath)
	if err != nil { return err }
	defer f.Close()
	var p PeerInfo
	if err := json.NewDecoder(f).Decode(&p); err != nil { return err }
	s.mu.Lock()
	s.peer = &p
	s.mu.Unlock()
	return nil
}

// WithCORS wraps a handler to add permissive CORS for browser clients.
func WithCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In dev we allow any origin. For production, set this to the specific origin(s).
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
// type turnState struct {
// 	MyTurn     string `json:"myTurn"`               // "me" | "opponent" | ""
// 	MyRootHex  string `json:"myRootHex,omitempty"`  // salted root (0x..)
// 	OppRootHex string `json:"oppRootHex,omitempty"` // salted root (0x..)
// 	Ready      bool   `json:"ready"`
// 	Decided    bool   `json:"decided"`
// 	MyID       string `json:"myId,omitempty"`       // e.g., "http://localhost:8080"
// 	OppID      string `json:"oppId,omitempty"`      // e.g., "http://localhost:8081"
// }

type turnState struct {
    MyTurn     string `json:"myTurn"`               // "me" | "opponent" | ""
    MyRootHex  string `json:"myRootHex,omitempty"`
    OppRootHex string `json:"oppRootHex,omitempty"`
    Ready      bool   `json:"ready"`
    Decided    bool   `json:"decided"`
    MyID       string `json:"myId,omitempty"`
    OppID      string `json:"oppId,omitempty"`
}

// decide exactly once; prefer roots, else IDs
func decideOnce(t *turnState) {
    if t.Decided {
        // keep Ready in sync
        t.Ready = (t.MyRootHex != "" && t.OppRootHex != "") || (t.MyID != "" && t.OppID != "")
        return
    }
    if t.MyRootHex != "" && t.OppRootHex != "" {
        if strings.ToLower(t.MyRootHex) < strings.ToLower(t.OppRootHex) {
            t.MyTurn = "me"
        } else {
            t.MyTurn = "opponent"
        }
        t.Ready = true
        t.Decided = true
        return
    }
    if t.MyID != "" && t.OppID != "" {
        if strings.ToLower(t.MyID) < strings.ToLower(t.OppID) {
            t.MyTurn = "me"
        } else {
            t.MyTurn = "opponent"
        }
        t.Ready = true
        t.Decided = true
    }
}


func (s *Server) ensureTurn() error {
	if _, err := os.Stat(s.TurnPath); os.IsNotExist(err) {
		t := &turnState{MyTurn: "", Ready: false, Decided: false}
		return s.saveTurn(t)
	}
	return nil
}
func (s *Server) loadTurn() (*turnState, error) {
	f, err := os.Open(s.TurnPath)
	if err != nil { return nil, err }
	defer f.Close()
	var t turnState
	if err := json.NewDecoder(f).Decode(&t); err != nil { return nil, err }
	return &t, nil
}
func (s *Server) saveTurn(t *turnState) error {
	f, err := os.Create(s.TurnPath)
	if err != nil { return err }
	defer f.Close()
	return json.NewEncoder(f).Encode(t)
}

// decide exactly once; prefer roots, else IDs
// func decideOnce(t *turnState) {
// 	if t.Decided {
// 		// keep Ready in sync
// 		if (t.MyRootHex != "" && t.OppRootHex != "") || (t.MyID != "" && t.OppID != "") {
// 			t.Ready = true
// 		}
// 		return
// 	}
// 	if t.MyRootHex != "" && t.OppRootHex != "" {
// 		if strings.ToLower(t.MyRootHex) < strings.ToLower(t.OppRootHex) {
// 			t.MyTurn = "me"
// 		} else {
// 			t.MyTurn = "opponent"
// 		}
// 		t.Ready = true
// 		t.Decided = true
// 		return
// 	}
// 	if t.MyID != "" && t.OppID != "" {
// 		if strings.ToLower(t.MyID) < strings.ToLower(t.OppID) {
// 			t.MyTurn = "me"
// 		} else {
// 			t.MyTurn = "opponent"
// 		}
// 		t.Ready = true
// 		t.Decided = true
// 	}
// }

func normalizeID(sid string) string {
    sid = strings.TrimSpace(sid)
    sid = strings.TrimRight(sid, "/")
    return strings.ToLower(sid)
}

func (s *Server) updateTurn(mut func(*turnState)) (*turnState, error) {
    if err := s.ensureTurn(); err != nil { return nil, err }
    t, err := s.loadTurn()
    if err != nil { return nil, err }

    // Apply mutation (may set MyID/OppID, etc.)
    mut(t)

    myID  := normalizeID(t.MyID)
    oppID := normalizeID(t.OppID)
    haveIDs := myID != "" && oppID != ""

    online, oppStarted := false, int64(0)
    if haveIDs {
        online, oppStarted = s.peerStatus(oppID) // reads /v1/turn
    }

    // If already decided, never change who starts; just refresh connectivity flag.
    if t.Decided {
        t.Ready = haveIDs && online
        if err := s.saveTurn(t); err != nil { return nil, err }
        return t, nil
    }

    // Decide exactly once when BOTH have valid start timestamps
    myStarted := s.startAt
    if haveIDs && online && myStarted > 0 && oppStarted > 0 {
        var iStart bool
        if myStarted != oppStarted {
            iStart = myStarted < oppStarted // earlier server starts
        } else {
            // Tie (millisecond collision). Break deterministically by normalized IDs.
            iStart = myID < oppID
        }
        if iStart { t.MyTurn = "me" } else { t.MyTurn = "opponent" }
        t.Ready = true
        t.Decided = true
    } else {
        // Not ready to decide yet
        t.Ready = false
        t.Decided = false
        // leave MyTurn as-is (likely "")
    }

    if err := s.saveTurn(t); err != nil { return nil, err }
    return t, nil
}

// GET /v1/turn
func (s *Server) handleTurnGet(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet { w.WriteHeader(http.StatusMethodNotAllowed); return }
    t, err := s.loadTurn()
    if err != nil { writeJSON(w, 500, map[string]string{"error": err.Error()}); return }
    writeJSON(w, 200, map[string]any{
        "myTurn":     t.MyTurn,
        "myRootHex":  t.MyRootHex,
        "oppRootHex": t.OppRootHex,
        "ready":      t.Ready,
        "decided":    t.Decided,
        "myId":       t.MyID,
        "oppId":      t.OppID,
        // NEW (authoritative liveness marker):
        "startedAt":  s.startAt, // ms since epoch
    })
}


// POST /v1/turn/self  { "baseUrl": "http://localhost:8080" }
type turnSelfReq struct{ BaseURL string `json:"baseUrl"` }
func (s *Server) handleTurnSetSelf(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
	var req turnSelfReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.BaseURL)=="" {
		writeJSON(w, 400, map[string]string{"error":"bad json or missing baseUrl"}); return
	}
	t, err := s.updateTurn(func(t *turnState){ t.MyID = strings.TrimRight(req.BaseURL, "/") })
	if err != nil { writeJSON(w,500,map[string]string{"error":err.Error()}); return }

	// If opponent ID is set but not online, signal it explicitly
	if strings.TrimSpace(t.OppID) != "" && !t.Ready {
		writeJSON(w, 409, map[string]any{
			"error":   "opponent is offline; turns not decided",
			"myTurn":  t.MyTurn, "ready": t.Ready, "decided": t.Decided, "oppId": t.OppID,
		})
		return
	}
	writeJSON(w,200,map[string]any{"ok":true,"myTurn":t.MyTurn,"decided":t.Decided,"ready":t.Ready})
}


// POST /v1/turn/opponent  { "rootHex":"0x..", "baseUrl":"http://localhost:8081" }
type turnOppReq struct {
	RootHex string `json:"rootHex,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
}
func (s *Server) handleTurnSetOppRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
	var req turnOppReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error":"bad json"}); return
	}
	t, err := s.updateTurn(func(t *turnState){
		if strings.TrimSpace(req.RootHex) != "" { t.OppRootHex = req.RootHex }
		if strings.TrimSpace(req.BaseURL) != "" { t.OppID = strings.TrimRight(req.BaseURL, "/") }
	})
	if err != nil { writeJSON(w,500,map[string]string{"error":err.Error()}); return }

	if !t.Ready {
		// Either opponent baseUrl is unknown, or peer is offline; in both cases do not decide turns
		msg := "opponent is offline or baseUrl not set; turns not decided"
		if strings.TrimSpace(t.OppID) == "" {
			msg = "opponent baseUrl not set; turns not decided"
		}
		writeJSON(w, 409, map[string]any{
			"error":   msg,
			"myTurn":  t.MyTurn, "ready": t.Ready, "decided": t.Decided, "oppId": t.OppID,
		})
		return
	}
	writeJSON(w,200,map[string]any{"ok":true,"myTurn":t.MyTurn,"decided":t.Decided,"ready":t.Ready})
}


// POST /v1/turn/next   (attacker calls after local verify succeeds)
func (s *Server) handleTurnNext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
	t, err := s.updateTurn(func(t *turnState){ t.MyTurn = "opponent" })
	if err != nil { writeJSON(w,500,map[string]string{"error":err.Error()}); return }
	writeJSON(w,200,map[string]any{"ok":true,"myTurn":t.MyTurn})
}







// ShotEvent is recorded when THIS server is the defender and receives /v1/shoot.
type ShotEvent struct {
    Row int   `json:"row"`
    Col int   `json:"col"`
    Bit uint8 `json:"bit"` // 0 miss, 1 hit
    N   int   `json:"n"`   // monotonic counter
    At  int64 `json:"at"`  // unix ms
}

func (s *Server) loadLastShot() (*ShotEvent, error) {
    f, err := os.Open(s.ShotsPath)
    if err != nil { return nil, err }
    defer f.Close()
    var ev ShotEvent
    if err := json.NewDecoder(f).Decode(&ev); err != nil { return nil, err }
    return &ev, nil
}

func (s *Server) saveLastShot(ev *ShotEvent) error {
    f, err := os.Create(s.ShotsPath)
    if err != nil { return err }
    defer f.Close()
    return json.NewEncoder(f).Encode(ev)
}

func (s *Server) recordShot(row, col int, bit uint8) {
    s.mu.Lock()
    defer s.mu.Unlock()
    n := 1
    if s.lastEvt != nil { n = s.lastEvt.N + 1 }
    ev := &ShotEvent{
        Row: row, Col: col, Bit: bit, N: n,
        At: time.Now().UnixMilli(),
    }
    _ = s.saveLastShot(ev) // best-effort
    s.lastEvt = ev
}


func (s *Server) handleDefenseLast(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet { w.WriteHeader(http.StatusMethodNotAllowed); return }
    s.mu.RLock()
    ev := s.lastEvt
    s.mu.RUnlock()
    if ev == nil {
        writeJSON(w, 200, map[string]any{"n": 0})
        return
    }
    writeJSON(w, 200, ev)
}







type gameState struct {
    HitsTaken int    `json:"hitsTaken"` // opponent hit my ships (defense)
    HitsDealt int    `json:"hitsDealt"` // I hit opponent ships (attack verify)
    Over      bool   `json:"over"`
    Winner    string `json:"winner"`    // "me" | "opponent" | ""
}

func (s *Server) ensureGame() error {
    if _, err := os.Stat(s.GamePath); os.IsNotExist(err) {
        return s.saveGame(&gameState{})
    }
    return nil
}
func (s *Server) loadGame() (*gameState, error) {
    f, err := os.Open(s.GamePath)
    if err != nil { return nil, err }
    defer f.Close()
    var g gameState
    if err := json.NewDecoder(f).Decode(&g); err != nil { return nil, err }
    return &g, nil
}
func (s *Server) saveGame(g *gameState) error {
    f, err := os.Create(s.GamePath)
    if err != nil { return err }
    defer f.Close()
    return json.NewEncoder(f).Encode(g)
}
func (s *Server) updateGame(mut func(*gameState)) (*gameState, error) {
    if err := s.ensureGame(); err != nil { return nil, err }
    g, err := s.loadGame()
    if err != nil { return nil, err }
    mut(g)
    if err := s.saveGame(g); err != nil { return nil, err }
    return g, nil
}


func (s *Server) handleGameState(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet { w.WriteHeader(http.StatusMethodNotAllowed); return }
    g, err := s.loadGame()
    if err != nil { writeJSON(w,500,map[string]string{"error":err.Error()}); return }
    writeJSON(w,200,g)
}

func (s *Server) handleGameReset(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
    _, err := s.updateGame(func(g *gameState){ *g = gameState{} })
    if err != nil { writeJSON(w,500,map[string]string{"error":err.Error()}); return }
    writeJSON(w,200,map[string]any{"ok":true})
}

func shotKey(r, c int) string { return fmt.Sprintf("%d,%d", r, c) }


// new: use /v1/turn (always exists)
func (s *Server) ping(baseURL string) bool {
    client := &http.Client{Timeout: 1500 * time.Millisecond}
    resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/v1/turn")
    if err != nil { return false }
    defer resp.Body.Close()
    return resp.StatusCode == http.StatusOK
}

func (s *Server) peerStatus(baseURL string) (online bool, startedAt int64) {
    if strings.TrimSpace(baseURL) == "" { return false, 0 }
    url := strings.TrimRight(baseURL, "/") + "/v1/turn"
    client := &http.Client{Timeout: 1500 * time.Millisecond}
    resp, err := client.Get(url)
    if err != nil { return false, 0 }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK { return false, 0 }

    var m map[string]any
    if err := json.NewDecoder(resp.Body).Decode(&m); err != nil { return false, 0 }

    // Require a valid, non-zero startedAt; otherwise treat as "not ready"
    if v, ok := m["startedAt"].(float64); ok && int64(v) > 0 {
        return true, int64(v)
    }
    return false, 0
}

