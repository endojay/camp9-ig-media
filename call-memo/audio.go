package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const telRate = 8000 // 전화 음성 표본화 주파수

// encodeWav 는 16비트 모노 PCM 표본을 WAV 파일 바이트로 만든다.
func encodeWav(samples []int16, rate int) []byte {
	var b bytes.Buffer
	dataLen := len(samples) * 2
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // 모노
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	binary.Write(&b, binary.LittleEndian, samples)
	return b.Bytes()
}

// decodeWav 는 PCM WAV(8/16/24/32비트 정수 또는 32비트 실수)를 읽어 모노 16비트 표본으로 돌려준다.
func decodeWav(data []byte) (samples []int16, rate int, err error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, errors.New("WAV 파일이 아닙니다")
	}
	var format, channels, bits uint16
	var pcm []byte
	for p := 12; p+8 <= len(data); {
		id := string(data[p : p+4])
		size := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
		body := p + 8
		end := body + size
		if end > len(data) || size < 0 {
			end = len(data) // 길이가 잘못 적힌 파일도 읽을 수 있는 만큼 읽는다
		}
		switch id {
		case "fmt ":
			if end-body < 16 {
				return nil, 0, errors.New("WAV 형식 정보가 손상되었습니다")
			}
			format = binary.LittleEndian.Uint16(data[body:])
			channels = binary.LittleEndian.Uint16(data[body+2:])
			rate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bits = binary.LittleEndian.Uint16(data[body+14:])
			if format == 0xFFFE && end-body >= 26 { // WAVE_FORMAT_EXTENSIBLE
				format = binary.LittleEndian.Uint16(data[body+24:])
			}
		case "data":
			pcm = data[body:end]
		}
		p = end + size%2
	}
	if pcm == nil || channels == 0 || rate <= 0 {
		return nil, 0, errors.New("WAV 파일에 음성 데이터가 없습니다")
	}
	if format != 1 && format != 3 {
		return nil, 0, fmt.Errorf("지원하지 않는 WAV 압축 형식입니다 (형식 코드 %d)", format)
	}
	bytesPer := int(bits) / 8
	if bytesPer < 1 || bytesPer > 4 || (format == 3 && bytesPer != 4) {
		return nil, 0, fmt.Errorf("지원하지 않는 WAV 비트 수입니다 (%d비트)", bits)
	}
	frame := bytesPer * int(channels)
	n := len(pcm) / frame
	samples = make([]int16, n)
	for i := 0; i < n; i++ {
		var sum float64
		for ch := 0; ch < int(channels); ch++ {
			s := pcm[i*frame+ch*bytesPer:]
			var v float64
			switch {
			case format == 3:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(s))) * 32767
			case bytesPer == 1:
				v = (float64(s[0]) - 128) * 256
			case bytesPer == 2:
				v = float64(int16(binary.LittleEndian.Uint16(s)))
			case bytesPer == 3:
				v = float64(int32(uint32(s[0])<<8|uint32(s[1])<<16|uint32(s[2])<<24)>>8) / 256
			default:
				v = float64(int32(binary.LittleEndian.Uint32(s))) / 65536
			}
			sum += v
		}
		samples[i] = clip16(sum / float64(channels))
	}
	return samples, rate, nil
}

func clip16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(math.Round(v))
}

// resample 은 표본화 주파수를 바꾼다. 낮출 때는 구간 평균으로 고음 잡음을 줄인다.
func resample(in []int16, from, to int) []int16 {
	if from == to || len(in) == 0 {
		return in
	}
	n := int(int64(len(in)) * int64(to) / int64(from))
	out := make([]int16, n)
	ratio := float64(from) / float64(to)
	for i := range out {
		pos := float64(i) * ratio
		if ratio > 1 {
			a, b := int(pos), int(pos+ratio)
			if b > len(in) {
				b = len(in)
			}
			if b <= a {
				b = a + 1
			}
			var sum float64
			for _, s := range in[a:b] {
				sum += float64(s)
			}
			out[i] = clip16(sum / float64(b-a))
			continue
		}
		a := int(pos)
		b := a + 1
		if b >= len(in) {
			b = len(in) - 1
		}
		frac := pos - float64(a)
		out[i] = clip16(float64(in[a])*(1-frac) + float64(in[b])*frac)
	}
	return out
}

// toTelephoneWav 는 어떤 WAV 든 전화로 틀 수 있는 8kHz 모노 16비트 WAV 로 바꾼다.
func toTelephoneWav(data []byte) ([]byte, float64, error) {
	samples, rate, err := decodeWav(data)
	if err != nil {
		return nil, 0, err
	}
	samples = resample(samples, rate, telRate)
	if len(samples) < telRate/2 {
		return nil, 0, errors.New("안내 멘트가 너무 짧습니다")
	}
	if len(samples) > telRate*120 {
		return nil, 0, errors.New("안내 멘트는 2분을 넘을 수 없습니다")
	}
	return encodeWav(samples, telRate), float64(len(samples)) / telRate, nil
}

// beepPCM 은 녹음 시작을 알리는 "삐" 소리(리틀엔디언 16비트 PCM)를 만든다.
func beepPCM() []byte {
	const ms = 450
	n := telRate * ms / 1000
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		env := 1.0
		if fade := telRate / 100; i < fade { // 10ms 페이드로 딸깍 소리 방지
			env = float64(i) / float64(fade)
		} else if i > n-fade {
			env = float64(n-i) / float64(fade)
		}
		v := math.Sin(2*math.Pi*1000*float64(i)/telRate) * 9000 * env
		binary.Write(&b, binary.LittleEndian, int16(v))
	}
	// 삐 소리 앞뒤로 짧은 무음
	pad := make([]byte, telRate/5*2)
	return append(append(append([]byte{}, pad...), b.Bytes()...), pad...)
}

// rms 는 리틀엔디언 16비트 PCM 조각의 소리 크기를 구한다.
func rms(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}
