//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

const canSynthesize = false

func openBrowser(url string) { exec.Command("xdg-open", url).Start() }

func synthesizeSpeech(text string) ([]byte, error) {
	return nil, errors.New("글자로 안내 멘트 만들기는 윈도우에서만 쓸 수 있습니다")
}

func autostartEnabled() bool { return false }

func setAutostart(on bool) error {
	return errors.New("자동 시작 설정은 윈도우에서만 쓸 수 있습니다")
}
