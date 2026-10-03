package main

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
)

// Сколько id помнить на раздел. Старые вытесняются: объявление месячной
// давности на первую страницу выдачи, отсортированной по дате, не вернётся.
const seenLimitPerCategory = 5000

type SentMessage struct {
	ChatID    ChatID `json:"chat_id"`
	MessageID int    `json:"message_id"`
	SentAt    int64  `json:"ts"`
	Category  string `json:"category,omitempty"`
	ListingID string `json:"listing,omitempty"`
}

type stateFile struct {
	Seen        map[string][]string `json:"seen"`
	Topics      map[string]int      `json:"topics"`
	Initialized []string            `json:"initialized_searches"`
	Sent        []SentMessage       `json:"sent"`
}

// State — всё, что переживает перезапуск, в одном файле.
type State struct {
	path        string
	seenOrder   map[string][]string
	seenSet     map[string]map[string]struct{}
	initialized map[string]struct{}
	Topics      map[string]int
	Sent        []SentMessage
	dirty       bool
}

func newState(path string) *State {
	return &State{
		path:        path,
		seenOrder:   make(map[string][]string),
		seenSet:     make(map[string]map[string]struct{}),
		initialized: make(map[string]struct{}),
		Topics:      make(map[string]int),
	}
}

func loadState(path string) (*State, error) {
	s := newState(path)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f stateFile
	if err := json.Unmarshal(data, &f); err != nil {
		broken := path + ".broken"
		_ = os.Rename(path, broken)
		logf("%s повреждён (%v), отложен в %s. Начинаю с чистого листа — первый проход пройдёт молча.", path, err, broken)
		return s, nil
	}
	for cat, ids := range f.Seen {
		for _, id := range ids {
			s.MarkSeen(cat, id)
		}
	}
	for k, v := range f.Topics {
		s.Topics[k] = v
	}
	for _, k := range f.Initialized {
		s.initialized[k] = struct{}{}
	}
	s.Sent = f.Sent
	s.dirty = false
	return s, nil
}

func (s *State) IsSeen(cat, id string) bool {
	_, ok := s.seenSet[cat][id]
	return ok
}

func (s *State) MarkSeen(cat, id string) {
	set := s.seenSet[cat]
	if set == nil {
		set = make(map[string]struct{})
		s.seenSet[cat] = set
	}
	if _, ok := set[id]; ok {
		return
	}
	set[id] = struct{}{}
	order := append(s.seenOrder[cat], id)
	// Подрезаем пачкой, а не по одному, чтобы не копировать список на каждом id.
	if len(order) > seenLimitPerCategory+500 {
		drop := len(order) - seenLimitPerCategory
		for _, old := range order[:drop] {
			delete(set, old)
		}
		order = append([]string(nil), order[drop:]...)
	}
	s.seenOrder[cat] = order
	s.dirty = true
}

// Поиск считается инициализированным после первого удачного прохода.
// Ключ — раздел + ссылка, поэтому новая ссылка в старом разделе тоже
// проходит первый раз молча, а не вываливает в чат всю выдачу.
func (s *State) IsInitialized(key string) bool {
	_, ok := s.initialized[key]
	return ok
}

func (s *State) MarkInitialized(key string) {
	s.initialized[key] = struct{}{}
	s.dirty = true
}

func (s *State) AddSent(m SentMessage) {
	s.Sent = append(s.Sent, m)
	s.dirty = true
}

// Save пишет через временный файл: если процесс убьют посреди записи,
// старый state.json останется целым.
func (s *State) Save() error {
	f := stateFile{Seen: s.seenOrder, Topics: s.Topics, Sent: s.Sent}
	for k := range s.initialized {
		f.Initialized = append(f.Initialized, k)
	}
	sort.Strings(f.Initialized)
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
