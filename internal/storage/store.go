package storage

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

func ID() string           { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:20] }

type Draft struct {
	ID      string    `json:"id"`
	Base    string    `json:"base"`
	Source  string    `json:"source"`
	Created time.Time `json:"created"`
}
type Revision struct {
	ID        string    `json:"id"`
	Digest    string    `json:"digest"`
	Parent    string    `json:"parent,omitempty"`
	Activated time.Time `json:"activated"`
}
type Store struct {
	Dir     string
	mu      sync.RWMutex
	active  string
	history []Revision
	traces  []Trace
}
type Trace struct {
	RunID      string    `json:"run_id"`
	Revision   string    `json:"revision"`
	Surface    string    `json:"surface"`
	Tool       string    `json:"tool"`
	Node       string    `json:"node,omitempty"`
	Status     string    `json:"status"`
	Message    string    `json:"message,omitempty"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}
type Effect struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Node      string    `json:"node"`
	Operation string    `json:"operation"`
	Status    string    `json:"status"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason,omitempty"`
}

func New(dir, initial string) (*Store, error) {
	for _, p := range []string{"revisions", "drafts", "layouts", "suspended", "tasks"} {
		if e := os.MkdirAll(filepath.Join(dir, p), 0700); e != nil {
			return nil, e
		}
	}
	s := &Store{Dir: dir}
	b, e := os.ReadFile(filepath.Join(dir, "history.json"))
	if e == nil {
		if e = json.Unmarshal(b, &s.history); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if len(s.history) > 0 {
		s.active = s.history[len(s.history)-1].ID
	} else {
		if _, e = s.activateUnsafe(initial); e != nil {
			return nil, e
		}
	}
	// Reload observability records after restart. Corrupt trailing JSONL lines
	// (e.g. interrupted disk writes) are ignored rather than crashing startup.
	s.traces = loadTraces(filepath.Join(dir, "traces.jsonl"))
	if e := s.RecoverInterruptedTasks(); e != nil {
		return nil, e
	}
	return s, nil
}
func atomicWrite(path string, b []byte) error {
	tmp := path + "." + ID() + ".tmp"
	if e := os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	if e := os.Rename(tmp, path); e != nil {
		_ = os.Remove(tmp)
		return e
	}
	return nil
}
func (s *Store) activateUnsafe(source string) (Revision, error) {
	rev := Revision{ID: Hash(source + ID()), Digest: Hash(source), Parent: s.active, Activated: time.Now().UTC()}
	if e := atomicWrite(filepath.Join(s.Dir, "revisions", rev.ID+".hcl"), []byte(source)); e != nil {
		return Revision{}, e
	}
	hist := append(append([]Revision{}, s.history...), rev)
	b, _ := json.MarshalIndent(hist, "", "  ")
	if e := atomicWrite(filepath.Join(s.Dir, "history.json"), b); e != nil {
		return Revision{}, e
	}
	s.history = hist
	s.active = rev.ID
	return rev, nil
}
func (s *Store) Active() (Revision, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.history) == 0 {
		return Revision{}, "", errors.New("no revisions")
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "revisions", s.active+".hcl"))
	return s.history[len(s.history)-1], string(b), e
}
func (s *Store) Source(id string) (string, error) {
	if len(id) != 20 {
		return "", fmt.Errorf("invalid revision id")
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "revisions", id+".hcl"))
	return string(b), e
}
func (s *Store) DraftCreate() (Draft, error) {
	rev, src, e := s.Active()
	if e != nil {
		return Draft{}, e
	}
	d := Draft{ID: ID(), Base: rev.ID, Source: src, Created: time.Now().UTC()}
	return d, s.saveDraft(d)
}
func (s *Store) saveDraft(d Draft) error {
	b, _ := json.MarshalIndent(d, "", "  ")
	return atomicWrite(filepath.Join(s.Dir, "drafts", d.ID+".json"), b)
}
func (s *Store) DraftGet(id string) (Draft, error) {
	if len(id) != 24 {
		return Draft{}, fmt.Errorf("invalid draft id")
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "drafts", id+".json"))
	if e != nil {
		return Draft{}, e
	}
	var d Draft
	e = json.Unmarshal(b, &d)
	return d, e
}
func (s *Store) DraftUpdate(id, source string) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, e := s.DraftGet(id)
	if e != nil {
		return d, e
	}
	d.Source = source
	return d, s.saveDraft(d)
}
func (s *Store) Activate(id string) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, e := s.DraftGet(id)
	if e != nil {
		return Revision{}, e
	}
	if s.active != d.Base {
		return Revision{}, fmt.Errorf("revision conflict: draft based on %s, current %s", d.Base, s.active)
	}
	r, e := s.activateUnsafe(d.Source)
	if e == nil {
		_ = os.Remove(filepath.Join(s.Dir, "drafts", id+".json"))
	}
	return r, e
}
func (s *Store) Rollback(target string) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(target) != 20 {
		return Revision{}, fmt.Errorf("invalid revision ID")
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "revisions", target+".hcl"))
	if e != nil {
		return Revision{}, e
	}
	return s.activateUnsafe(string(b))
}
func (s *Store) History() []Revision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Revision{}, s.history...)
}
func (s *Store) WriteTrace(t Trace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.At.IsZero() {
		t.At = time.Now().UTC()
	}
	s.traces = append(s.traces, t)
	if len(s.traces) > 2000 {
		s.traces = s.traces[len(s.traces)-2000:]
	}
	_ = appendLine(filepath.Join(s.Dir, "traces.jsonl"), t)
}
func (s *Store) Traces() []Trace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := append([]Trace{}, s.traces...)
	sort.SliceStable(res, func(i, j int) bool { return res[i].At.After(res[j].At) })
	return res
}

// Effects returns the latest durable status per effect identifier. The event
// log is append-only, and we never infer that an unknown effect failed.
func (s *Store) Effects() []Effect {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, err := os.Open(filepath.Join(s.Dir, "effects.jsonl"))
	if err != nil {
		return []Effect{}
	}
	defer f.Close()
	latest := map[string]Effect{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var event Effect
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.ID != "" {
			latest[event.ID] = event
		}
	}
	events := make([]Effect, 0, len(latest))
	for _, e := range latest {
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	return events
}

func loadTraces(path string) []Trace {
	f, err := os.Open(path)
	if err != nil {
		return []Trace{}
	}
	defer f.Close()
	traces := []Trace{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var item Trace
		if json.Unmarshal(scanner.Bytes(), &item) == nil && item.RunID != "" {
			traces = append(traces, item)
			if len(traces) > 2000 {
				traces = traces[1:]
			}
		}
	}
	return traces
}

func (s *Store) Effect(e Effect) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return appendLine(filepath.Join(s.Dir, "effects.jsonl"), e)
}
func appendLine(path string, v any) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if _, e = f.Write(append(b, '\n')); e != nil {
		return e
	}
	return f.Sync()
}
func (s *Store) PutSuspended(id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return atomicWrite(filepath.Join(s.Dir, "suspended", id+".json"), b)
}
func (s *Store) GetSuspended(id string, v any) error {
	if len(id) != 24 {
		return fmt.Errorf("invalid run ID")
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "suspended", id+".json"))
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func (s *Store) DeleteSuspended(id string) error {
	if len(id) != 24 {
		return fmt.Errorf("invalid run ID")
	}
	return os.Remove(filepath.Join(s.Dir, "suspended", id+".json"))
}
func (s *Store) Layout(name string) (map[string]any, error) {
	if !safeName(name) {
		return nil, fmt.Errorf("invalid name")
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "layouts", name+".json"))
	if errors.Is(e, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if e != nil {
		return nil, e
	}
	var v map[string]any
	e = json.Unmarshal(b, &v)
	return v, e
}
func (s *Store) SaveLayout(name string, v map[string]any) error {
	if !safeName(name) {
		return fmt.Errorf("invalid name")
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(filepath.Join(s.Dir, "layouts", name+".json"), b)
}
func safeName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
