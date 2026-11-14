package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	cfgs    *configStore
	memos   *memoStore
	engine  *Engine
	dataDir string
	addr    string // "127.0.0.1:포트"
	quit    func()
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))

	mux.HandleFunc("GET /api/state", s.getState)
	mux.HandleFunc("GET /api/memos", s.listMemos)
	mux.HandleFunc("POST /api/memos", s.addMemo)
	mux.HandleFunc("PATCH /api/memos/{id}", s.patchMemo)
	mux.HandleFunc("DELETE /api/memos/{id}", s.deleteMemo)
	mux.HandleFunc("POST /api/memos/{id}/transcribe", s.retranscribe)
	mux.HandleFunc("GET /api/recordings/{id}", s.getRecording)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("GET /api/greeting", s.getGreeting)
	mux.HandleFunc("POST /api/greeting", s.postGreeting)
	mux.HandleFunc("POST /api/greeting/tts", s.ttsGreeting)
	mux.HandleFunc("DELETE /api/greeting", s.deleteGreeting)
	mux.HandleFunc("PUT /api/autostart", s.putAutostart)
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); s.quit() }()
	})
	return s.guard(mux)
}

// guard 는 이 PC 의 화면에서 온 요청만 받아들인다 (다른 웹사이트가 몰래 호출하는 것을 막는다).
func (s *server) guard(next http.Handler) http.Handler {
	_, port, _ := strings.Cut(s.addr, ":")
	hosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !hosts[strings.TrimPrefix(o, "http://")] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CallMemo") == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (s *server) getState(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgs.get()
	n, rev := s.memos.summary()
	writeJSON(w, map[string]any{
		"status":      s.engine.Status(),
		"newCount":    n,
		"rev":         rev,
		"activeCalls": s.engine.active.Load(),
		"answering":   cfg.shouldAnswer(time.Now()),
		"mode":        cfg.Mode,
		"clinicName":  cfg.ClinicName,
	})
}

func (s *server) listMemos(w http.ResponseWriter, r *http.Request) {
	list := s.memos.list()
	if list == nil {
		list = []Memo{}
	}
	writeJSON(w, list)
}

func (s *server) addMemo(w http.ResponseWriter, r *http.Request) {
	var in struct{ Caller, Name, Note string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(in.Caller+in.Name+in.Note) == "" {
		writeErr(w, 400, errors.New("내용을 입력해 주세요"))
		return
	}
	caller := strings.TrimSpace(in.Caller)
	if caller != "" {
		caller = formatPhone(caller)
	}
	m, err := s.memos.add(Memo{Caller: caller, Name: strings.TrimSpace(in.Name), Note: strings.TrimSpace(in.Note), Source: "manual"})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, m)
}

func (s *server) patchMemo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name, Note, Status, Text *string
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if in.Status != nil && *in.Status != "new" && *in.Status != "seen" && *in.Status != "done" {
		writeErr(w, 400, errors.New("알 수 없는 상태입니다"))
		return
	}
	m, err := s.memos.update(r.PathValue("id"), func(m *Memo) {
		if in.Name != nil {
			m.Name = *in.Name
		}
		if in.Note != nil {
			m.Note = *in.Note
		}
		if in.Text != nil {
			m.Text = *in.Text
		}
		if in.Status != nil {
			m.Status = *in.Status
			m.DoneAt = time.Time{}
			if m.Status == "done" {
				m.DoneAt = time.Now()
			}
		}
	})
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, m)
}

func (s *server) deleteMemo(w http.ResponseWriter, r *http.Request) {
	if err := s.memos.remove(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) retranscribe(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgs.get()
	if !cfg.STTEnabled || cfg.STTKey == "" {
		writeErr(w, 400, errors.New("설정에서 음성 → 글자 변환을 먼저 켜 주세요"))
		return
	}
	id := r.PathValue("id")
	m, err := s.memos.update(id, func(m *Memo) {
		if m.Recording != "" {
			m.TextState = "pending"
		}
	})
	if err != nil || m.Recording == "" {
		writeErr(w, 404, errors.New("녹음이 없는 메모입니다"))
		return
	}
	go s.engine.transcribeMemo(id, cfg)
	writeJSON(w, m)
}

func (s *server) getRecording(w http.ResponseWriter, r *http.Request) {
	m, ok := s.memos.get(r.PathValue("id"))
	if !ok || m.Recording == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeFile(w, r, s.memos.recordingPath(m.Recording))
}

// configView 는 화면으로 보내는 설정이다. 비밀번호와 키는 보내지 않고 저장 여부만 알려준다.
type configView struct {
	Config
	HasPassword bool   `json:"hasPassword"`
	HasSTTKey   bool   `json:"hasSttKey"`
	Autostart   bool   `json:"autostart"`
	CanTTS      bool   `json:"canTts"`
	DataDir     string `json:"dataDir"`
}

func (s *server) getConfig(w http.ResponseWriter, r *http.Request) {
	c := s.cfgs.get()
	v := configView{Config: c, HasPassword: c.SIPPassword != "", HasSTTKey: c.STTKey != "",
		Autostart: autostartEnabled(), CanTTS: canSynthesize, DataDir: s.dataDir}
	v.SIPPassword, v.STTKey = "", ""
	if v.Holidays == nil {
		v.Holidays = []string{}
	}
	writeJSON(w, v)
}

func (s *server) putConfig(w http.ResponseWriter, r *http.Request) {
	old := s.cfgs.get()
	in := old
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	// 비밀번호 칸을 비워 두면 기존 값을 유지한다.
	if in.SIPPassword == "" {
		in.SIPPassword = old.SIPPassword
	}
	if in.STTKey == "" {
		in.STTKey = old.STTKey
	}
	if err := s.cfgs.set(in); err != nil {
		writeErr(w, 500, err)
		return
	}
	now := s.cfgs.get()
	if now.SIPServer != old.SIPServer || now.SIPUser != old.SIPUser || now.SIPAuthUser != old.SIPAuthUser ||
		now.SIPPassword != old.SIPPassword || now.SIPProxy != old.SIPProxy ||
		now.SIPTransport != old.SIPTransport || now.LocalPort != old.LocalPort {
		s.engine.Restart()
	}
	s.getConfig(w, r)
}

func (s *server) getGreeting(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(s.engine.greetingPath()); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeFile(w, r, s.engine.greetingPath())
}

func (s *server) saveGreeting(w http.ResponseWriter, wav []byte) {
	out, secs, err := toTelephoneWav(wav)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := writeFileAtomic(s.engine.greetingPath(), out); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "seconds": secs})
}

func (s *server) postGreeting(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.saveGreeting(w, b)
}

func (s *server) ttsGreeting(w http.ResponseWriter, r *http.Request) {
	var in struct{ Text string }
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Text) == "" {
		writeErr(w, 400, errors.New("안내 문구를 입력해 주세요"))
		return
	}
	wav, err := synthesizeSpeech(in.Text)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	cfg := s.cfgs.get()
	cfg.GreetingText = in.Text
	s.cfgs.set(cfg)
	s.saveGreeting(w, wav)
}

func (s *server) deleteGreeting(w http.ResponseWriter, r *http.Request) {
	os.Remove(s.engine.greetingPath())
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) putAutostart(w http.ResponseWriter, r *http.Request) {
	var in struct{ Enabled bool }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := setAutostart(in.Enabled); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"autostart": autostartEnabled()})
}
