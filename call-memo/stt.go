package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// transcribe 는 녹음 파일을 OpenAI 호환 음성 인식 API 로 보내 글자로 바꾼다.
func transcribe(ctx context.Context, cfg Config, wavPath string) (string, error) {
	audio, err := os.ReadFile(wavPath)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("model", cfg.STTModel)
	w.WriteField("language", "ko")
	w.WriteField("response_format", "json")
	part, err := w.CreateFormFile("file", "voicemail.wav")
	if err != nil {
		return "", err
	}
	part.Write(audio)
	w.Close()

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.STTEndpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+cfg.STTKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("음성 인식 서버 오류 %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("음성 인식 응답을 읽을 수 없습니다: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}
