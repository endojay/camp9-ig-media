package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func tone(seconds float64, hz float64) []int16 {
	out := make([]int16, int(seconds*telRate))
	for i := range out {
		out[i] = int16(math.Sin(2*math.Pi*hz*float64(i)/telRate) * 8000)
	}
	return out
}

func testEngine(t *testing.T, mut func(*Config)) (*Engine, *memoStore) {
	t.Helper()
	dir := t.TempDir()
	cfgs, _ := loadConfig(dir)
	cfg := cfgs.get()
	cfg.Mode, cfg.AnswerDelay, cfg.SilenceSec, cfg.MaxRecordSec = "always", 0, 3, 20
	mut(&cfg)
	if err := cfgs.set(cfg); err != nil {
		t.Fatal(err)
	}
	memos, _ := loadMemos(dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := newEngine(cfgs, memos, dir, log)
	os.WriteFile(e.greetingPath(), encodeWav(tone(1, 440), telRate), 0o600)
	e.Restart()
	t.Cleanup(e.Stop)
	time.Sleep(300 * time.Millisecond)
	return e, memos
}

func testCaller(t *testing.T, port int) *diago.Diago {
	t.Helper()
	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("01012345678"))
	t.Cleanup(func() { ua.Close() })
	dg := diago.NewDiago(ua, diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: port}))
	return dg
}

// 시간 외 전화: 안내 멘트 → 삐 → 발신자 음성이 녹음되어 메모로 남아야 한다.
func TestVoicemailRecorded(t *testing.T) {
	_, memos := testEngine(t, func(c *Config) { c.LocalPort = 15080 })
	dg := testCaller(t, 15090)

	var uri sip.Uri
	sip.ParseUri("sip:camp9@127.0.0.1:15080", &uri)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, med, err := dg.Invite(ctx, uri, diago.InviteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop, _ := med.ListenBackground()
	time.Sleep(2500 * time.Millisecond) // 안내 멘트(1초) + 삐 소리가 끝나기를 기다림
	pb, _ := med.PlaybackCreate()
	if _, err := pb.Play(bytes.NewReader(encodeWav(tone(2, 300), telRate)), "audio/wav"); err != nil {
		t.Fatal(err)
	}
	stop()
	d.Hangup(ctx)
	time.Sleep(700 * time.Millisecond)

	list := memos.list()
	if len(list) != 1 {
		t.Fatalf("메모 %d건, 1건이어야 함", len(list))
	}
	m := list[0]
	if m.Caller != "010-1234-5678" || m.Status != "new" || m.Source != "auto" {
		t.Fatalf("메모 내용 오류: %+v", m)
	}
	if m.Recording == "" || m.Duration < 1.5 || m.Duration > 3.5 {
		t.Fatalf("녹음 길이 오류: %q %.2f초", m.Recording, m.Duration)
	}
	b, _ := os.ReadFile(memos.recordingPath(m.Recording))
	samples, rate, err := decodeWav(b)
	if err != nil || rate != telRate {
		t.Fatal(err, rate)
	}
	raw := make([]byte, 0, len(samples)*2)
	for _, s := range samples {
		raw = append(raw, byte(s), byte(s>>8))
	}
	if level := rms(raw); level < 2000 {
		t.Fatalf("녹음에 발신자 음성이 없음 (크기 %.0f)", level)
	}
}

// 발신자가 말없이 있으면 설정한 침묵 시간 뒤 자동으로 끊고, 녹음 없는 부재중 기록만 남긴다.
func TestSilentCallerHungUp(t *testing.T) {
	_, memos := testEngine(t, func(c *Config) { c.LocalPort = 15081 })
	dg := testCaller(t, 15091)
	var uri sip.Uri
	sip.ParseUri("sip:camp9@127.0.0.1:15081", &uri)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, med, err := dg.Invite(ctx, uri, diago.InviteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stop, _ := med.ListenBackground()
	defer stop()
	pb, _ := med.PlaybackCreate()
	go pb.Play(bytes.NewReader(encodeWav(make([]int16, telRate*15), telRate)), "audio/wav") // 무음 전송
	select {
	case <-d.Context().Done():
	case <-time.After(12 * time.Second):
		t.Fatal("침묵이 이어져도 전화가 끊기지 않음")
	}
	time.Sleep(300 * time.Millisecond)
	list := memos.list()
	if len(list) != 1 || list[0].Recording != "" {
		t.Fatalf("부재중 기록 오류: %+v", list)
	}
}

// 진료시간 중에는 받지 않아야 한다 (원내 전화기가 받도록).
func TestBusinessHoursNotAnswered(t *testing.T) {
	_, memos := testEngine(t, func(c *Config) { c.LocalPort = 15082; c.Mode = "off" })
	dg := testCaller(t, 15092)
	var uri sip.Uri
	sip.ParseUri("sip:camp9@127.0.0.1:15082", &uri)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := dg.Invite(ctx, uri, diago.InviteOptions{}); err == nil {
		t.Fatal("받지 않아야 할 전화를 받음")
	}
	if n := len(memos.list()); n != 0 {
		t.Fatalf("메모가 %d건 생김", n)
	}
}

// 인터넷전화 서버에 등록되면 상태가 registered 가 되어야 한다.
func TestRegister(t *testing.T) {
	ua, _ := sipgo.NewUA()
	defer ua.Close()
	srv, _ := sipgo.NewServer(ua)
	got := make(chan string, 4)
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		got <- req.From().Address.String()
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.ListenAndServe(ctx, "udp", "127.0.0.1:15099")
	time.Sleep(200 * time.Millisecond)

	e, _ := testEngine(t, func(c *Config) {
		c.LocalPort = 15083
		c.SIPServer, c.SIPUser, c.SIPPassword = "127.0.0.1:15099", "07012345678", "pw"
	})
	select {
	case from := <-got:
		if from != "sip:07012345678@127.0.0.1" {
			t.Fatalf("From 주소 오류: %s", from)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("등록 요청이 오지 않음")
	}
	time.Sleep(300 * time.Millisecond)
	if st := e.Status(); st.State != "registered" {
		t.Fatalf("상태 %+v", st)
	}
}

func TestSchedule(t *testing.T) {
	c := defaultConfig()
	at := func(s string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		return v
	}
	cases := []struct {
		when string
		want bool // 자동응답해야 하는가
	}{
		{"2026-11-16 08:00", true},  // 월요일 진료 전
		{"2026-11-16 10:00", false}, // 월요일 진료 중
		{"2026-11-16 13:30", false}, // 점심 (기본: 받지 않음)
		{"2026-11-16 18:30", true},  // 진료 종료 시각
		{"2026-11-14 15:00", true},  // 토요일 오후
		{"2026-11-15 11:00", true},  // 일요일
	}
	for _, tc := range cases {
		if got := c.shouldAnswer(at(tc.when)); got != tc.want {
			t.Errorf("%s: 자동응답=%v, 기대=%v", tc.when, got, tc.want)
		}
	}
	c.AnswerAtLunch = true
	if !c.shouldAnswer(at("2026-11-16 13:30")) {
		t.Error("점심시간 자동응답 설정이 적용되지 않음")
	}
	c.Holidays = []string{"2026-11-17"}
	if !c.shouldAnswer(at("2026-11-17 10:00")) {
		t.Error("휴진일에 자동응답하지 않음")
	}
}

func TestWavConvert(t *testing.T) {
	// 44.1kHz 스테레오 → 8kHz 모노
	src := tone(1, 440)
	src = resample(src, telRate, 44100)
	var st bytes.Buffer
	st.Write(encodeWav(src, 44100))
	out, secs, err := toTelephoneWav(st.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	s, rate, _ := decodeWav(out)
	if rate != telRate || math.Abs(secs-1) > 0.05 || len(s) < 7900 {
		t.Fatalf("변환 오류 rate=%d secs=%.2f n=%d", rate, secs, len(s))
	}
	if _, _, err := toTelephoneWav([]byte("not a wav")); err == nil {
		t.Fatal("잘못된 파일을 받아들임")
	}
}
