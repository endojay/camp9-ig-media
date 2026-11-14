package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DayHours 는 요일 하루의 진료시간이다. 시간은 "HH:MM" 형식.
type DayHours struct {
	Open       bool   `json:"open"`
	Start      string `json:"start"`
	End        string `json:"end"`
	LunchStart string `json:"lunchStart"`
	LunchEnd   string `json:"lunchEnd"`
}

type Config struct {
	ClinicName string `json:"clinicName"`

	// 인터넷전화(SIP) 계정
	SIPServer    string `json:"sipServer"`    // 등록 서버 주소 (도메인 또는 IP, 포트 포함 가능)
	SIPUser      string `json:"sipUser"`      // 전화번호 / 사용자 ID
	SIPAuthUser  string `json:"sipAuthUser"`  // 인증 ID (비우면 SIPUser 사용)
	SIPPassword  string `json:"sipPassword"`  // 비밀번호
	SIPProxy     string `json:"sipProxy"`     // 아웃바운드 프록시 (선택)
	SIPTransport string `json:"sipTransport"` // udp 또는 tcp
	LocalPort    int    `json:"localPort"`    // 이 PC가 전화를 기다리는 포트

	// 응답 방식: "schedule"(진료시간 외에만), "always"(항상), "off"(받지 않음)
	Mode          string `json:"mode"`
	AnswerDelay   int    `json:"answerDelay"`   // 벨이 울린 뒤 받기까지 초
	MaxRecordSec  int    `json:"maxRecordSec"`  // 최대 녹음 시간(초)
	SilenceSec    int    `json:"silenceSec"`    // 이 시간만큼 조용하면 녹음 종료(초)
	AnswerAtLunch bool   `json:"answerAtLunch"` // 점심시간에도 자동응답

	// 요일별 진료시간. 인덱스 0=일요일 ... 6=토요일
	Hours    [7]DayHours `json:"hours"`
	Holidays []string    `json:"holidays"` // 휴진일 "YYYY-MM-DD"

	GreetingText string `json:"greetingText"`

	// 음성 → 글자 변환(선택). OpenAI 호환 API.
	STTEnabled  bool   `json:"sttEnabled"`
	STTEndpoint string `json:"sttEndpoint"`
	STTModel    string `json:"sttModel"`
	STTKey      string `json:"sttKey"`
}

func defaultConfig() Config {
	weekday := DayHours{Open: true, Start: "09:30", End: "18:30", LunchStart: "13:00", LunchEnd: "14:00"}
	c := Config{
		ClinicName:   "캠프나인치과의원",
		SIPTransport: "udp",
		LocalPort:    5080,
		Mode:         "schedule",
		AnswerDelay:  3,
		MaxRecordSec: 120,
		SilenceSec:   8,
		GreetingText: "안녕하세요. 캠프나인치과의원입니다. 지금은 진료시간이 아닙니다. " +
			"삐 소리가 나면 성함과 연락처, 용건을 말씀해 주세요. 진료가 시작되면 연락드리겠습니다.",
		STTEndpoint: "https://api.openai.com/v1/audio/transcriptions",
		STTModel:    "whisper-1",
	}
	for i := 1; i <= 5; i++ {
		c.Hours[i] = weekday
	}
	c.Hours[6] = DayHours{Open: true, Start: "09:30", End: "14:00"}
	c.Hours[0] = DayHours{Open: false, Start: "09:30", End: "18:30"}
	return c
}

// normalize 는 잘못된 값을 안전한 기본값으로 바로잡는다.
func (c *Config) normalize() {
	c.SIPServer = strings.TrimSpace(c.SIPServer)
	c.SIPUser = strings.TrimSpace(c.SIPUser)
	c.SIPAuthUser = strings.TrimSpace(c.SIPAuthUser)
	c.SIPProxy = strings.TrimSpace(c.SIPProxy)
	if c.SIPTransport != "tcp" {
		c.SIPTransport = "udp"
	}
	if c.LocalPort < 1024 || c.LocalPort > 65535 {
		c.LocalPort = 5080
	}
	if c.Mode != "always" && c.Mode != "off" {
		c.Mode = "schedule"
	}
	c.AnswerDelay = clamp(c.AnswerDelay, 0, 30)
	c.MaxRecordSec = clamp(c.MaxRecordSec, 10, 600)
	c.SilenceSec = clamp(c.SilenceSec, 3, 60)
	hol := c.Holidays[:0:0]
	for _, h := range c.Holidays {
		h = strings.TrimSpace(h)
		if _, err := time.Parse("2006-01-02", h); err == nil {
			hol = append(hol, h)
		}
	}
	c.Holidays = hol
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parseHM 은 "HH:MM" 을 자정 이후 분으로 바꾼다. 형식이 틀리면 ok=false.
func parseHM(s string) (int, bool) {
	h, m, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// inBusinessHours 는 t 가 직원이 전화를 받을 수 있는 시간인지 알려준다.
func (c *Config) inBusinessHours(t time.Time) bool {
	date := t.Format("2006-01-02")
	for _, h := range c.Holidays {
		if h == date {
			return false
		}
	}
	d := c.Hours[int(t.Weekday())]
	if !d.Open {
		return false
	}
	start, ok1 := parseHM(d.Start)
	end, ok2 := parseHM(d.End)
	if !ok1 || !ok2 {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	if now < start || now >= end {
		return false
	}
	if c.AnswerAtLunch {
		ls, ok1 := parseHM(d.LunchStart)
		le, ok2 := parseHM(d.LunchEnd)
		if ok1 && ok2 && now >= ls && now < le {
			return false
		}
	}
	return true
}

// shouldAnswer 는 지금 걸려온 전화를 자동응답이 받아야 하는지 결정한다.
func (c *Config) shouldAnswer(t time.Time) bool {
	switch c.Mode {
	case "always":
		return true
	case "off":
		return false
	}
	return !c.inBusinessHours(t)
}

type configStore struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func loadConfig(dir string) (*configStore, error) {
	s := &configStore{path: filepath.Join(dir, "config.json"), cfg: defaultConfig()}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return nil, err
	}
	s.cfg.normalize()
	return s, nil
}

func (s *configStore) get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	c.Holidays = append([]string(nil), s.cfg.Holidays...)
	return c
}

func (s *configStore) set(c Config) error {
	c.normalize()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFileAtomic(s.path, b); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

// writeFileAtomic 은 쓰는 도중 전원이 꺼져도 기존 파일이 깨지지 않게 저장한다.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
