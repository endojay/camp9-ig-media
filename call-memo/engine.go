package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// EngineStatus 는 화면에 보여줄 전화 연결 상태이다.
type EngineStatus struct {
	State   string    `json:"state"` // unconfigured, connecting, registered, error, stopped
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// Engine 은 인터넷전화에 연결해 시간 외 전화를 받고 녹음한다.
type Engine struct {
	cfgs    *configStore
	memos   *memoStore
	dataDir string
	log     *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	status atomic.Value // EngineStatus
	active atomic.Int32 // 통화 중인 전화 수
}

func newEngine(cfgs *configStore, memos *memoStore, dataDir string, log *slog.Logger) *Engine {
	e := &Engine{cfgs: cfgs, memos: memos, dataDir: dataDir, log: log}
	e.setStatus("stopped", "")
	return e
}

func (e *Engine) setStatus(state, msg string) {
	e.status.Store(EngineStatus{State: state, Message: msg, Since: time.Now()})
}

func (e *Engine) Status() EngineStatus { return e.status.Load().(EngineStatus) }

func (e *Engine) greetingPath() string { return filepath.Join(e.dataDir, "greeting.wav") }

// Restart 는 현재 설정으로 전화 연결을 다시 시작한다.
func (e *Engine) Restart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		e.run(ctx, e.cfgs.get())
	}(e.done)
}

func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

func (e *Engine) stopLocked() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(8 * time.Second):
	}
	e.cancel = nil
	e.setStatus("stopped", "")
}

func (e *Engine) run(ctx context.Context, cfg Config) {
	domain := cfg.SIPServer
	if h, _, ok := strings.Cut(domain, ":"); ok {
		domain = h
	}
	uaOpts := []sipgo.UserAgentOption{}
	if cfg.SIPUser != "" {
		uaOpts = append(uaOpts, sipgo.WithUserAgent(cfg.SIPUser))
	}
	if domain != "" {
		uaOpts = append(uaOpts, sipgo.WithUserAgentHostname(domain))
	}
	ua, err := sipgo.NewUA(uaOpts...)
	if err != nil {
		e.setStatus("error", "전화 모듈을 시작하지 못했습니다: "+err.Error())
		return
	}
	defer ua.Close()

	dg := diago.NewDiago(ua,
		diago.WithLogger(e.log),
		diago.WithTransport(diago.Transport{
			Transport:      cfg.SIPTransport,
			BindHost:       "0.0.0.0",
			BindPort:       cfg.LocalPort,
			RewriteContact: true,
		}),
	)

	e.setStatus("connecting", "")
	if err := dg.ServeBackground(ctx, func(d *diago.DialogServerSession) { e.handleCall(d) }); err != nil {
		e.setStatus("error", fmt.Sprintf("포트 %d 을(를) 열 수 없습니다. 다른 전화 프로그램이 같은 포트를 쓰고 있는지 확인해 주세요. (%v)", cfg.LocalPort, err))
		<-ctx.Done()
		return
	}

	if cfg.SIPServer == "" || cfg.SIPUser == "" {
		e.setStatus("unconfigured", "인터넷전화 계정이 아직 입력되지 않았습니다.")
		<-ctx.Done()
		return
	}

	var recipient sip.Uri
	uri := fmt.Sprintf("sip:%s@%s", cfg.SIPUser, cfg.SIPServer)
	if cfg.SIPTransport == "tcp" {
		uri += ";transport=tcp"
	}
	if err := sip.ParseUri(uri, &recipient); err != nil {
		e.setStatus("error", "서버 주소 형식이 올바르지 않습니다: "+cfg.SIPServer)
		<-ctx.Done()
		return
	}
	authUser := cfg.SIPAuthUser
	if authUser == "" {
		authUser = cfg.SIPUser
	}

	// 등록이 끊기면 30초 뒤 다시 시도한다.
	for ctx.Err() == nil {
		err := dg.Register(ctx, recipient, diago.RegisterOptions{
			Username:     authUser,
			Password:     cfg.SIPPassword,
			ProxyHost:    cfg.SIPProxy,
			Expiry:       5 * time.Minute,
			OnRegistered: func() { e.setStatus("registered", "") },
		})
		if ctx.Err() != nil {
			return
		}
		e.log.Warn("전화 등록 실패", "error", err)
		e.setStatus("error", registerErrorMessage(err))
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

func registerErrorMessage(err error) string {
	var rr *diago.RegisterResponseError
	if errors.As(err, &rr) {
		switch rr.StatusCode() {
		case 401, 403, 407:
			return "인증에 실패했습니다. 인증 ID와 비밀번호를 확인해 주세요."
		case 404:
			return "서버에서 이 전화번호(사용자 ID)를 찾을 수 없습니다."
		}
		return "서버가 등록을 거절했습니다: " + rr.Error()
	}
	if err == nil {
		return "서버와의 연결이 끊어졌습니다. 잠시 후 다시 시도합니다."
	}
	return "서버에 연결할 수 없습니다. 서버 주소와 인터넷 연결을 확인해 주세요. (" + err.Error() + ")"
}

// callerNumber 는 발신 번호를 보기 좋은 국내 형식으로 돌려준다.
func callerNumber(d *diago.DialogServerSession) string {
	user := d.FromUser()
	if h := d.InviteRequest.GetHeader("P-Asserted-Identity"); h != nil {
		v := h.Value()
		if i := strings.Index(v, "sip:"); i >= 0 {
			if j := strings.IndexAny(v[i+4:], "@>;"); j > 0 {
				user = v[i+4 : i+4+j]
			}
		}
	}
	return formatPhone(user)
}

func formatPhone(s string) string {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	if s == "" || low == "anonymous" || low == "unknown" || low == "restricted" {
		return "발신번호 표시제한"
	}
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	n := digits.String()
	if n == "" {
		return s
	}
	if strings.HasPrefix(n, "82") && len(n) >= 11 {
		n = "0" + n[2:]
	}
	switch {
	case strings.HasPrefix(n, "02") && len(n) == 9:
		return n[:2] + "-" + n[2:5] + "-" + n[5:]
	case strings.HasPrefix(n, "02") && len(n) == 10:
		return n[:2] + "-" + n[2:6] + "-" + n[6:]
	case len(n) == 10:
		return n[:3] + "-" + n[3:6] + "-" + n[6:]
	case len(n) == 11:
		return n[:3] + "-" + n[3:7] + "-" + n[7:]
	case len(n) == 8:
		return n[:4] + "-" + n[4:]
	}
	return n
}

// 소리 크기가 이 값보다 작으면 말이 없는 것으로 본다 (16비트 최대값 32768 기준).
const silenceRMS = 250

func (e *Engine) handleCall(d *diago.DialogServerSession) {
	cfg := e.cfgs.get()
	caller := callerNumber(d)
	ctx := d.Context()
	log := e.log.With("caller", caller)

	d.Trying()
	if !cfg.shouldAnswer(time.Now()) {
		// 진료시간에는 받지 않고 원내 전화기가 받도록 둔다. 상대가 끊을 때까지 기다린다.
		log.Info("진료시간 중 전화 - 자동응답하지 않음")
		d.Ringing()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Minute):
		}
		return
	}

	e.active.Add(1)
	defer e.active.Add(-1)

	d.Ringing()
	select {
	case <-ctx.Done():
		return // 받기 전에 상대가 끊음
	case <-time.After(time.Duration(cfg.AnswerDelay) * time.Second):
	}

	med, err := d.Answer(diago.AnswerOptions{RTPNAT: media.RTPNATSymetric})
	if err != nil {
		log.Error("전화를 받지 못했습니다", "error", err)
		return
	}
	log.Info("자동응답 시작")

	memo, err := e.memos.add(Memo{Caller: caller, Source: "auto"})
	if err != nil {
		log.Error("메모 저장 실패", "error", err)
	}

	// 상대 음성을 처음부터 계속 읽는다. 안내 멘트 중의 소리는 버리고, 삐 소리 뒤부터 저장한다.
	var props diago.MediaProps
	reader, err := med.AudioReader(diago.WithAudioReaderMediaProps(&props))
	if err != nil {
		log.Error("음성을 읽을 수 없습니다", "error", err)
		return
	}
	decoder, err := audio.NewPCMDecoderReader(props.Codec.PayloadType, reader)
	if err != nil {
		log.Error("지원하지 않는 음성 형식입니다", "error", err)
		return
	}

	var (
		recording  atomic.Bool
		recMu      sync.Mutex
		pcm        bytes.Buffer
		voicedMS   int
		readerDone = make(chan struct{})
		silent     = make(chan struct{})
	)
	go func() {
		defer close(readerDone)
		buf := make([]byte, media.RTPBufSize*2)
		quietMS, silentSent := 0, false
		for {
			n, err := decoder.Read(buf)
			if err != nil {
				return
			}
			if n == 0 || !recording.Load() {
				continue
			}
			ms := n / 2 * 1000 / telRate
			recMu.Lock()
			pcm.Write(buf[:n])
			if rms(buf[:n]) >= silenceRMS {
				voicedMS += ms
				quietMS = 0
			} else {
				quietMS += ms
			}
			recMu.Unlock()
			if !silentSent && quietMS >= cfg.SilenceSec*1000 {
				silentSent = true
				close(silent)
			}
		}
	}()

	if pb, err := med.PlaybackCreate(); err == nil {
		if greeting, err := os.ReadFile(e.greetingPath()); err == nil {
			if _, err := pb.Play(bytes.NewReader(greeting), "audio/wav"); err != nil {
				log.Warn("안내 멘트 재생 실패", "error", err)
			}
		}
		if ctx.Err() == nil {
			pb.Play(bytes.NewReader(beepPCM()), "audio/pcm")
		}
	}

	if ctx.Err() == nil {
		recording.Store(true)
		select {
		case <-ctx.Done():
		case <-readerDone:
		case <-silent:
		case <-time.After(time.Duration(cfg.MaxRecordSec) * time.Second):
		}
		recording.Store(false)
	}

	hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	d.Hangup(hctx)
	cancel()

	recMu.Lock()
	data := append([]byte(nil), pcm.Bytes()...)
	voiced := voicedMS
	recMu.Unlock()

	if memo.ID == "" {
		return
	}
	if voiced < 500 {
		log.Info("음성 메시지 없이 종료")
		return // 메모는 "메시지 없음" 부재중 기록으로 남는다
	}

	samples := make([]int16, len(data)/2)
	binary.Read(bytes.NewReader(data), binary.LittleEndian, samples)
	name := memo.ID + ".wav"
	if err := os.WriteFile(e.memos.recordingPath(name), encodeWav(samples, telRate), 0o600); err != nil {
		log.Error("녹음 저장 실패", "error", err)
		return
	}
	duration := float64(len(samples)) / telRate
	sttOn := cfg.STTEnabled && cfg.STTKey != ""
	e.memos.update(memo.ID, func(m *Memo) {
		m.Recording = name
		m.Duration = duration
		if sttOn {
			m.TextState = "pending"
		}
	})
	log.Info("음성 메모 저장", "seconds", duration)

	if sttOn {
		go e.transcribeMemo(memo.ID, cfg)
	}
}

func (e *Engine) transcribeMemo(id string, cfg Config) {
	m, ok := e.memos.get(id)
	if !ok || m.Recording == "" {
		return
	}
	text, err := transcribe(context.Background(), cfg, e.memos.recordingPath(m.Recording))
	e.memos.update(id, func(m *Memo) {
		if err != nil {
			m.TextState = "error"
			e.log.Warn("음성 인식 실패", "error", err)
			return
		}
		m.Text = text
		m.TextState = "done"
	})
}
