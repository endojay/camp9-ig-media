package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const canSynthesize = true

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "Camp9CallMemo"

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func openBrowser(url string) {
	hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", url)).Start()
}

// synthesizeSpeech 는 윈도우에 내장된 음성 합성으로 안내 멘트 WAV 를 만든다.
func synthesizeSpeech(text string) ([]byte, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("callmemo-tts-%d.wav", os.Getpid()))
	defer os.Remove(out)
	script := `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Speech
$s = New-Object System.Speech.Synthesis.SpeechSynthesizer
$ko = $s.GetInstalledVoices() | Where-Object { $_.Enabled -and $_.VoiceInfo.Culture.Name -eq 'ko-KR' } | Select-Object -First 1
if (-not $ko) { Write-Error 'NO_KOREAN_VOICE'; exit 3 }
$s.SelectVoice($ko.VoiceInfo.Name)
$s.Rate = -1
$fmt = New-Object System.Speech.AudioFormat.SpeechAudioFormatInfo(16000, [System.Speech.AudioFormat.AudioBitsPerSample]::Sixteen, [System.Speech.AudioFormat.AudioChannel]::Mono)
$s.SetOutputToWaveFile($env:CALLMEMO_OUT, $fmt)
$s.Speak($env:CALLMEMO_TEXT)
$s.Dispose()
`
	cmd := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script))
	cmd.Env = append(os.Environ(), "CALLMEMO_OUT="+out, "CALLMEMO_TEXT="+text)
	if b, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(b), "NO_KOREAN_VOICE") {
			return nil, errors.New("이 PC에 한국어 음성이 설치되어 있지 않습니다. 윈도우 설정 → 시간 및 언어 → 음성에서 한국어 음성을 추가하거나, 마이크 녹음을 이용해 주세요")
		}
		return nil, fmt.Errorf("음성 합성에 실패했습니다: %v %s", err, strings.TrimSpace(string(b)))
	}
	return os.ReadFile(out)
}

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runName)
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runName, `"`+exe+`" -no-browser`)
}
