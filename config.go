package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// ChatID принимает chat_id и строкой, и числом: "-100123" и -100123 равнозначны.
type ChatID string

func (c *ChatID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*c = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
	}
	*c = ChatID(s)
	return nil
}

// Range — пауза в секундах: одно число (30) или диапазон ([20, 40]),
// из которого каждый раз берётся случайное значение.
type Range struct{ Min, Max float64 }

func (r *Range) UnmarshalJSON(b []byte) error {
	var one float64
	if err := json.Unmarshal(b, &one); err == nil {
		r.Min, r.Max = one, one
		return nil
	}
	var two []float64
	if err := json.Unmarshal(b, &two); err != nil || len(two) == 0 || len(two) > 2 {
		return fmt.Errorf("пауза задаётся числом или парой [от, до], а получено %s", b)
	}
	r.Min, r.Max = two[0], two[len(two)-1]
	if r.Max < r.Min {
		r.Min, r.Max = r.Max, r.Min
	}
	return nil
}

// Filters — правила отбора. Указатели отличают «не задано» (тогда берётся
// значение из defaults) от явно заданного нуля или false.
type Filters struct {
	Comment              json.RawMessage `json:"_comment,omitempty"`
	MinPrice             *int            `json:"min_price"`
	MaxPrice             *int            `json:"max_price"`
	MaxAgeMinutes        *int            `json:"max_age_minutes"`
	NotifyWithoutPrice   *bool           `json:"notify_without_price"`
	NotifyWithoutAge     *bool           `json:"notify_without_age"`
	NotifyOnFirstRun     *bool           `json:"notify_on_first_run"`
	IncludeInDescription *bool           `json:"include_in_description"`
	TitleInclude         []string        `json:"title_include"`
	TitleExclude         []string        `json:"title_exclude"`
	DescriptionExclude   []string        `json:"description_exclude"`
}

// CategoryConfig — раздел из конфига: свои поиски, свои фильтры, своя тема.
type CategoryConfig struct {
	Filters
	Comment    json.RawMessage `json:"_comment,omitempty"`
	Name       string          `json:"name"`
	SearchURLs []string        `json:"search_urls"`
	ChatID     ChatID          `json:"telegram_chat_id"`
	TopicID    int             `json:"topic_id"`
	Disabled   bool            `json:"disabled"`
}

type Config struct {
	Comment          json.RawMessage `json:"_comment,omitempty"`
	TelegramBotToken string          `json:"telegram_bot_token"`
	TelegramChatID   ChatID          `json:"telegram_chat_id"`
	TelegramProxy    string          `json:"telegram_proxy"`
	AutoCreateTopics *bool           `json:"auto_create_topics"`

	PollIntervalSeconds      int      `json:"poll_interval_seconds"`
	MinPollIntervalSeconds   int      `json:"min_poll_interval_seconds"`
	MaxPollIntervalSeconds   int      `json:"max_poll_interval_seconds"`
	AdaptiveInterval         *bool    `json:"adaptive_interval"`
	TargetSuccessRate        float64  `json:"target_success_rate"`
	StartupDelaySeconds      *int     `json:"startup_delay_seconds"`
	SessionTTLMinutes        *int     `json:"session_ttl_minutes"`
	CategoriesPerCycle       int      `json:"categories_per_cycle"`
	PauseBetweenSearches     *Range   `json:"pause_between_searches_seconds"`
	PauseBetweenCategories   *Range   `json:"pause_between_categories_seconds"`
	PhotoDelay               *Range   `json:"photo_delay_seconds"`
	MaxNotificationsPerCycle int      `json:"max_notifications_per_cycle"`
	SendPhoto                *bool    `json:"send_photo"`
	ShowTime                 *bool    `json:"show_time"`
	DeleteAfterHours         *float64 `json:"delete_after_hours"`

	Proxies     []string `json:"proxies"`
	ProxiesFile string   `json:"proxies_file"`

	Defaults   Filters          `json:"defaults"`
	Categories []CategoryConfig `json:"categories"`

	// Вычисляются в applyDefaults.
	autoTopics      bool
	adaptive        bool
	sendPhoto       bool
	showTime        bool
	pauseSearches   Range
	pauseCategories Range
	photoDelay      Range
	startupDelay    time.Duration
	sessionTTL      time.Duration
	deleteAfter     time.Duration
}

// Category — раздел после слияния с defaults, готовый к работе.
type Category struct {
	Name            string
	SearchURLs      []string
	ChatID          ChatID
	TopicID         int
	topicFromConfig bool

	MinPrice             int
	MaxPrice             int           // 0 — без верхней границы
	MaxAge               time.Duration // 0 — любой давности
	NotifyWithoutPrice   bool
	NotifyWithoutAge     bool
	NotifyOnFirstRun     bool
	IncludeInDescription bool
	Include              []Matcher
	Exclude              []Matcher
	DescExclude          []Matcher
}

func (c *Category) topicKey() string { return string(c.ChatID) + "/" + c.Name }

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("не найден %s: скопируйте config.example.json в config.json и заполните его", path)
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	// Опечатка в имени ключа («title_exlude») молча выключила бы фильтр,
	// поэтому неизвестный ключ — это ошибка, а не тишина.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) && se.Offset <= int64(len(data)) {
			line := 1 + bytes.Count(data[:se.Offset], []byte("\n"))
			return nil, fmt.Errorf("%s: ошибка JSON в строке %d: %v", path, line, err)
		}
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if !strings.Contains(cfg.TelegramBotToken, ":") {
		return nil, fmt.Errorf("%s: не заполнен telegram_bot_token (он выглядит как 123456:ABC…)", path)
	}
	if cfg.TelegramChatID == "" {
		return nil, fmt.Errorf("%s: не заполнен telegram_chat_id", path)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.PollIntervalSeconds <= 0 {
		c.PollIntervalSeconds = 90
	}
	if c.MinPollIntervalSeconds <= 0 {
		c.MinPollIntervalSeconds = 45
	}
	if c.MaxPollIntervalSeconds < c.MinPollIntervalSeconds {
		c.MaxPollIntervalSeconds = max(600, c.MinPollIntervalSeconds)
	}
	if c.TargetSuccessRate <= 0 || c.TargetSuccessRate > 1 {
		c.TargetSuccessRate = 0.85
	}
	if c.CategoriesPerCycle <= 0 {
		c.CategoriesPerCycle = 1
	}
	if c.MaxNotificationsPerCycle <= 0 {
		c.MaxNotificationsPerCycle = 8
	}
	c.autoTopics = boolOr(c.AutoCreateTopics, true)
	c.adaptive = boolOr(c.AdaptiveInterval, true)
	c.sendPhoto = boolOr(c.SendPhoto, true)
	c.showTime = boolOr(c.ShowTime, true)
	c.pauseSearches = rangeOr(c.PauseBetweenSearches, Range{3, 6})
	c.pauseCategories = rangeOr(c.PauseBetweenCategories, Range{20, 40})
	c.photoDelay = rangeOr(c.PhotoDelay, Range{1, 3})
	c.startupDelay = time.Duration(max(0, intOr(c.StartupDelaySeconds, 30))) * time.Second
	c.sessionTTL = time.Duration(max(0, intOr(c.SessionTTLMinutes, 45))) * time.Minute

	// Telegram разрешает боту удалять свои сообщения только 48 часов. При
	// пороге ровно 48 часть сообщений выпадала бы из окна между проверками.
	hours := 47.0
	if c.DeleteAfterHours != nil {
		hours = *c.DeleteAfterHours
	}
	if hours >= 48 {
		logf("delete_after_hours=%g: Telegram не даёт боту удалять сообщения старше 48 ч, ставлю 47.", hours)
		hours = 47
	}
	c.deleteAfter = time.Duration(max(0, hours) * float64(time.Hour))
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func rangeOr(p *Range, def Range) Range {
	if p == nil {
		return def
	}
	return *p
}

// mergeFilters накладывает настройки раздела на общие. Белый список раздела
// заменяет общий, а чёрные списки складываются: общий отсекает аксессуары
// и клоны для всех, свой — то, что мешает именно этому разделу.
func mergeFilters(def, own Filters) Filters {
	out := def
	if own.MinPrice != nil {
		out.MinPrice = own.MinPrice
	}
	if own.MaxPrice != nil {
		out.MaxPrice = own.MaxPrice
	}
	if own.MaxAgeMinutes != nil {
		out.MaxAgeMinutes = own.MaxAgeMinutes
	}
	if own.NotifyWithoutPrice != nil {
		out.NotifyWithoutPrice = own.NotifyWithoutPrice
	}
	if own.NotifyWithoutAge != nil {
		out.NotifyWithoutAge = own.NotifyWithoutAge
	}
	if own.NotifyOnFirstRun != nil {
		out.NotifyOnFirstRun = own.NotifyOnFirstRun
	}
	if own.IncludeInDescription != nil {
		out.IncludeInDescription = own.IncludeInDescription
	}
	if len(own.TitleInclude) > 0 {
		out.TitleInclude = own.TitleInclude
	}
	out.TitleExclude = append(append([]string{}, def.TitleExclude...), own.TitleExclude...)
	out.DescriptionExclude = append(append([]string{}, def.DescriptionExclude...), own.DescriptionExclude...)
	return out
}

func buildCategories(cfg *Config) ([]*Category, error) {
	var out []*Category
	names := make(map[string]bool)
	for i, cc := range cfg.Categories {
		if cc.Disabled {
			continue
		}
		name := strings.TrimSpace(cc.Name)
		if name == "" {
			name = fmt.Sprintf("Раздел %d", i+1)
		}
		if names[name] {
			return nil, fmt.Errorf("два раздела с именем «%s»: имена должны различаться", name)
		}
		names[name] = true

		urls := cleanURLs(name, cc.SearchURLs)
		if len(urls) == 0 {
			logf("[%s] раздел пропущен: нет search_urls", name)
			continue
		}
		f := mergeFilters(cfg.Defaults, cc.Filters)
		c := &Category{
			Name:                 name,
			SearchURLs:           urls,
			ChatID:               cc.ChatID,
			TopicID:              cc.TopicID,
			topicFromConfig:      cc.TopicID != 0,
			MinPrice:             intOr(f.MinPrice, 0),
			MaxPrice:             intOr(f.MaxPrice, 0),
			MaxAge:               time.Duration(intOr(f.MaxAgeMinutes, 120)) * time.Minute,
			NotifyWithoutPrice:   boolOr(f.NotifyWithoutPrice, true),
			NotifyWithoutAge:     boolOr(f.NotifyWithoutAge, false),
			NotifyOnFirstRun:     boolOr(f.NotifyOnFirstRun, false),
			IncludeInDescription: boolOr(f.IncludeInDescription, false),
		}
		if c.ChatID == "" {
			c.ChatID = cfg.TelegramChatID
		}
		var err error
		if c.Include, err = buildMatchers(f.TitleInclude, name+"/title_include"); err != nil {
			return nil, err
		}
		if c.Exclude, err = buildMatchers(f.TitleExclude, name+"/title_exclude"); err != nil {
			return nil, err
		}
		if c.DescExclude, err = buildMatchers(f.DescriptionExclude, name+"/description_exclude"); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("в конфиге нет ни одного раздела с search_urls")
	}
	return out, nil
}

func cleanURLs(name string, raw []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || !strings.HasSuffix(p.Hostname(), "avito.ru") {
			logf("[%s] пропускаю ссылку — это не поиск Авито: %s", name, u)
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
