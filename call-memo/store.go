package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Memo 는 걸려온 전화 한 건의 기록이다.
type Memo struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Caller    string    `json:"caller"`
	Name      string    `json:"name"`      // 환자 이름 (직원이 입력)
	Source    string    `json:"source"`    // "auto" 자동응답, "manual" 직원 입력
	Duration  float64   `json:"duration"`  // 녹음 길이(초)
	Recording string    `json:"recording"` // 녹음 파일명 (없으면 빈 값)
	Text      string    `json:"text"`      // 음성을 글자로 바꾼 내용
	TextState string    `json:"textState"` // "", "pending", "done", "error"
	Note      string    `json:"note"`      // 직원 메모
	Status    string    `json:"status"`    // "new", "seen", "done"
	DoneAt    time.Time `json:"doneAt"`
}

type memoStore struct {
	mu    sync.Mutex
	path  string
	recs  string // 녹음 폴더
	memos []Memo
	rev   int // 내용이 바뀔 때마다 1씩 늘어난다 (화면 새로고침 판단용)
}

func loadMemos(dir string) (*memoStore, error) {
	s := &memoStore{path: filepath.Join(dir, "memos.json"), recs: filepath.Join(dir, "recordings")}
	if err := os.MkdirAll(s.recs, 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.memos); err != nil {
		return nil, err
	}
	return s, nil
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return time.Now().Format("20060102-150405-") + hex.EncodeToString(b)
}

func (s *memoStore) saveLocked() error {
	s.rev++
	b, err := json.MarshalIndent(s.memos, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

// list 는 최근 것이 먼저 오도록 복사본을 돌려준다.
func (s *memoStore) list() []Memo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Memo(nil), s.memos...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

func (s *memoStore) add(m Memo) (Memo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = newID()
	}
	if m.At.IsZero() {
		m.At = time.Now()
	}
	if m.Status == "" {
		m.Status = "new"
	}
	s.memos = append(s.memos, m)
	return m, s.saveLocked()
}

var errNotFound = errors.New("메모를 찾을 수 없습니다")

// update 는 id 의 메모를 f 로 고친 뒤 저장한다.
func (s *memoStore) update(id string, f func(*Memo)) (Memo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.memos {
		if s.memos[i].ID == id {
			f(&s.memos[i])
			return s.memos[i], s.saveLocked()
		}
	}
	return Memo{}, errNotFound
}

func (s *memoStore) get(id string) (Memo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.memos {
		if m.ID == id {
			return m, true
		}
	}
	return Memo{}, false
}

func (s *memoStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.memos {
		if m.ID == id {
			if m.Recording != "" {
				os.Remove(filepath.Join(s.recs, filepath.Base(m.Recording)))
			}
			s.memos = append(s.memos[:i], s.memos[i+1:]...)
			return s.saveLocked()
		}
	}
	return errNotFound
}

func (s *memoStore) recordingPath(name string) string {
	return filepath.Join(s.recs, filepath.Base(name))
}

// summary 는 새 메모 수와 현재 변경 번호를 돌려준다.
func (s *memoStore) summary() (n, rev int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.memos {
		if m.Status == "new" {
			n++
		}
	}
	return n, s.rev
}
