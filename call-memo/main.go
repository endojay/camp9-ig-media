// 전화 메모 — 진료시간 외에 걸려온 전화를 자동으로 받아 음성 메모로 남기는 프로그램.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

func main() {
	defaultDir := "data"
	if d, err := os.UserConfigDir(); err == nil {
		defaultDir = filepath.Join(d, "Camp9CallMemo")
	}
	dataDir := flag.String("data", defaultDir, "메모와 설정을 저장할 폴더")
	port := flag.Int("port", 8765, "화면(웹) 포트")
	noBrowser := flag.Bool("no-browser", false, "시작할 때 화면을 열지 않음")
	flag.Parse()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	url := "http://" + addr + "/"

	// 이미 실행 중이면 화면만 열고 끝낸다.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if !*noBrowser {
			openBrowser(url)
		}
		return
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fatal("저장 폴더를 만들 수 없습니다: " + err.Error())
	}
	logPath := filepath.Join(*dataDir, "app.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 5<<20 {
		os.Rename(logPath, logPath+".old")
	}
	var logOut io.Writer = os.Stderr
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		defer f.Close()
		logOut = io.MultiWriter(f, os.Stderr)
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfgs, err := loadConfig(*dataDir)
	if err != nil {
		fatal("설정 파일을 읽을 수 없습니다: " + err.Error())
	}
	memos, err := loadMemos(*dataDir)
	if err != nil {
		fatal("메모 파일을 읽을 수 없습니다: " + err.Error())
	}

	engine := newEngine(cfgs, memos, *dataDir, log)
	engine.Restart()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := &server{cfgs: cfgs, memos: memos, engine: engine, dataDir: *dataDir, addr: addr, quit: stop}
	httpSrv := &http.Server{Handler: srv.handler()}
	go httpSrv.Serve(ln)
	log.Info("전화 메모 시작", "url", url, "data", *dataDir)

	if !*noBrowser {
		openBrowser(url)
	}

	<-ctx.Done()
	engine.Stop()
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	httpSrv.Shutdown(sctx)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
