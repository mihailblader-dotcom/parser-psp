package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	baseBackoff     = 30 * time.Second
	maxBackoff      = 15 * time.Minute // если банят, частые попытки только усугубляют
	maxSendFailures = 3                // после стольких неудач подряд объявление пропускается
)

type counters struct{ requests, blocked, failed, listings, sent int }

type Watcher struct {
	cfg      *Config
	cats     []*Category
	state    *State
	tg       *Telegram
	avito    *AvitoClient
	proxies  []*url.URL
	proxyIdx int
	debug    bool
	perCycle int

	interval    time.Duration
	minInterval time.Duration
	maxInterval time.Duration
	recent      []bool // исходы последних циклов для адаптивного интервала

	backoff           time.Duration
	consecutiveBlocks int
	cursor            int
	failures          map[string]int
	warnedNoAge       map[string]bool
	stats             counters
	lastReport        time.Time
}

func newWatcher(cfg *Config, cats []*Category, st *State, tg *Telegram, proxies []*url.URL, debug bool) *Watcher {
	w := &Watcher{
		cfg:         cfg,
		cats:        cats,
		state:       st,
		tg:          tg,
		proxies:     proxies,
		debug:       debug,
		perCycle:    cfg.CategoriesPerCycle,
		minInterval: time.Duration(cfg.MinPollIntervalSeconds) * time.Second,
		maxInterval: time.Duration(cfg.MaxPollIntervalSeconds) * time.Second,
		backoff:     baseBackoff,
		failures:    make(map[string]int),
		warnedNoAge: make(map[string]bool),
		lastReport:  time.Now(),
	}
	w.interval = clampDuration(time.Duration(cfg.PollIntervalSeconds)*time.Second, w.minInterval, w.maxInterval)
	return w
}

func (w *Watcher) Run(ctx context.Context, once bool) error {
	if once {
		w.perCycle = len(w.cats) // проверка настроек — пройти все разделы сразу
	}
	w.logSetup()
	w.newSession(ctx, false)
	w.avito.CheckExitIP(ctx)
	w.avito.Warm(ctx)
	w.resolveTopics(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}

	// Прогрев, проверка IP и сразу выдача — это всплеск запросов, на который
	// лимитер Авито отвечает пачкой 429. Короткая выдержка убирает отказы на старте.
	if d := w.cfg.startupDelay; d > 0 {
		logf("Пауза %s перед первым поиском, чтобы не попасть под лимит на старте.", d)
		if err := sleepCtx(ctx, d); err != nil {
			return err
		}
	}

	logf("Watcher запущен. Ctrl+C (или systemctl stop) для остановки.")
	for {
		penalty := w.cycle(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		w.afterCycle(ctx)
		if once {
			logf("Режим -once: проход завершён.")
			return nil
		}
		wait := penalty
		if wait <= 0 {
			wait = w.jitteredInterval()
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

func (w *Watcher) logSetup() {
	for _, c := range w.cats {
		price := fmt.Sprintf("от %d", c.MinPrice)
		if c.MaxPrice > 0 {
			price += fmt.Sprintf(" до %d", c.MaxPrice)
		}
		age := "любой давности"
		if c.MaxAge > 0 {
			age = "не старше " + humanDuration(c.MaxAge)
		}
		logf("[%s] поисков: %d | цена %s ₽ | %s | белый список: %d, чёрный: %d (+%d по тексту)",
			c.Name, len(c.SearchURLs), price, age, len(c.Include), len(c.Exclude), len(c.DescExclude))
	}
	if len(w.proxies) > 0 {
		logf("Прокси в пуле: %d, работаю через них с ротацией.", len(w.proxies))
	} else {
		logf("Список прокси пуст — работаю напрямую.")
	}
	if n := len(w.cats); n > w.perCycle {
		every := time.Duration(float64(w.interval) * float64(n) / float64(w.perCycle))
		logf("За цикл проверяю разделов: %d — каждый раздел опрашивается примерно раз в %s.", w.perCycle, humanDuration(every))
	}
	if w.cfg.deleteAfter > 0 {
		logf("Сообщения старше %s удаляются автоматически.", humanDuration(w.cfg.deleteAfter))
	}
}

// ---------------------------------------------------------------------------
// Темы Telegram
// ---------------------------------------------------------------------------

func (w *Watcher) resolveTopics(ctx context.Context) {
	for _, c := range w.cats {
		switch {
		case c.TopicID != 0:
			// задана в конфиге
		case w.state.Topics[c.topicKey()] != 0:
			c.TopicID = w.state.Topics[c.topicKey()]
		case w.cfg.autoTopics:
			if err := w.createTopic(ctx, c); err != nil {
				logf("[%s] не удалось создать тему: %v. Объявления пойдут в общий поток. "+
					"Нужно: в группе включены «Темы», бот — администратор с правом «Управление темами».", c.Name, err)
			}
		}
		if c.TopicID != 0 {
			logf("[%s] → тема %d", c.Name, c.TopicID)
		} else {
			logf("[%s] → общий поток чата", c.Name)
		}
	}
}

func (w *Watcher) createTopic(ctx context.Context, c *Category) error {
	id, err := w.tg.CreateTopic(ctx, c.ChatID, c.Name)
	if err != nil {
		return err
	}
	c.TopicID = id
	w.state.Topics[c.topicKey()] = id
	w.state.dirty = true
	logf("[%s] создана тема в Telegram (topic_id %d)", c.Name, id)
	return w.state.Save()
}

// recoverTopic вызывается, когда Telegram говорит, что темы нет (её удалили).
func (w *Watcher) recoverTopic(ctx context.Context, c *Category) int {
	logf("[%s] тема %d не найдена в чате — похоже, её удалили", c.Name, c.TopicID)
	old := c.TopicID
	c.TopicID = 0
	if c.topicFromConfig || !w.cfg.autoTopics {
		logf("[%s] до перезапуска шлю в общий поток. Проверьте topic_id %d в конфиге.", c.Name, old)
		return 0
	}
	delete(w.state.Topics, c.topicKey())
	w.state.dirty = true
	if err := w.createTopic(ctx, c); err != nil {
		logf("[%s] новую тему создать не удалось: %v. Шлю в общий поток.", c.Name, err)
	}
	return c.TopicID
}

// ---------------------------------------------------------------------------
// Цикл опроса
// ---------------------------------------------------------------------------

// cycle — один проход по очередной порции разделов. Возвращает паузу-штраф
// (0, если всё прошло гладко и ждать надо обычный интервал).
func (w *Watcher) cycle(ctx context.Context) time.Duration {
	n := min(w.perCycle, len(w.cats))
	start := w.cursor
	w.cursor = (w.cursor + n) % len(w.cats)

	// Одинаковые ссылки у разных разделов скачиваются один раз за цикл:
	// можно завести одну широкую выдачу и раскладывать её по темам фильтрами.
	cache := make(map[string][]*Listing)
	fetchedBefore := false
	for i := 0; i < n; i++ {
		c := w.cats[(start+i)%len(w.cats)]
		listings, fetched, err := w.fetchCategory(ctx, c, cache, fetchedBefore)
		fetchedBefore = fetchedBefore || fetched > 0
		if ctx.Err() != nil {
			return 0
		}
		if err != nil {
			w.recordOutcome(false)
			return w.handleFetchError(ctx, err)
		}
		w.process(ctx, c, listings)
		if ctx.Err() != nil {
			return 0
		}
	}
	// Успех записывается один раз за цикл (в Python-версии — дважды).
	w.recordOutcome(true)
	w.consecutiveBlocks = 0
	w.backoff = baseBackoff
	return 0
}

func (w *Watcher) fetchCategory(ctx context.Context, c *Category, cache map[string][]*Listing, fetchedBefore bool) ([]*Listing, int, error) {
	var all []*Listing
	got := make(map[string]bool)
	fetched := 0
	for _, u := range c.SearchURLs {
		items, cached := cache[u]
		if !cached {
			var pause time.Duration
			switch {
			case fetched > 0:
				pause = randDuration(w.cfg.pauseSearches)
			case fetchedBefore:
				// Несколько секунд между разделами лимитер считает всплеском.
				pause = randDuration(w.cfg.pauseCategories)
			}
			if err := sleepCtx(ctx, pause); err != nil {
				return nil, fetched, err
			}
			var err error
			items, err = w.avito.FetchSearch(ctx, u)
			fetched++
			w.stats.requests++
			if err != nil {
				return nil, fetched, fmt.Errorf("[%s] поиск %s: %w", c.Name, shortSearch(u), err)
			}
			cache[u] = items
			w.stats.listings += len(items)
		}

		// Инициализируем только по непустой выдаче: если первой пришла
		// заглушка антибота, следующая нормальная страница не должна уйти в чат.
		key := c.Name + " | " + u
		if !w.state.IsInitialized(key) && len(items) > 0 {
			if !c.NotifyOnFirstRun {
				for _, l := range items {
					w.state.MarkSeen(c.Name, l.ID)
				}
				logf("[%s] первый проход по поиску %s: запомнил %d объявлений, не отправляя их",
					c.Name, shortSearch(u), len(items))
			}
			w.state.MarkInitialized(key)
		}
		// Поиски пересекаются, поэтому схлопываем по id.
		for _, l := range items {
			if !got[l.ID] {
				got[l.ID] = true
				all = append(all, l)
			}
		}
	}
	// Пустая выдача по всем поискам раздела — почти наверняка заглушка
	// антибота или новая вёрстка. Считаем это сбоем, а не удачным циклом:
	// иначе адаптивный интервал ускорялся бы ровно тогда, когда нас блокируют.
	if len(all) == 0 {
		return nil, fetched, fmt.Errorf("[%s] %w", c.Name, ErrNoItems)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Published.After(all[j].Published) })
	w.checkAges(c, all)
	return all, fetched, nil
}

// checkAges громко предупреждает, если время публикации перестало
// определяться: иначе фильтр по возрасту тихо отбрасывал бы всё подряд.
func (w *Watcher) checkAges(c *Category, items []*Listing) {
	if c.MaxAge <= 0 {
		return
	}
	known := 0
	for _, l := range items {
		if !l.Published.IsZero() {
			known++
		}
	}
	switch {
	case known == 0 && !w.warnedNoAge[c.Name]:
		w.warnedNoAge[c.Name] = true
		what := "такие объявления отбрасываются (notify_without_age: false)"
		if c.NotifyWithoutAge {
			what = "фильтр по возрасту не работает (notify_without_age: true)"
		}
		logf("[%s] ВНИМАНИЕ: ни у одного объявления не определилось время публикации — похоже, "+
			"Авито поменял вёрстку. Запустите с -debug и посмотрите debug_last_page.html. Пока так, %s.", c.Name, what)
	case known > 0 && w.warnedNoAge[c.Name]:
		w.warnedNoAge[c.Name] = false
		logf("[%s] время публикации снова определяется.", c.Name)
	}
}

func (w *Watcher) process(ctx context.Context, c *Category, listings []*Listing) {
	now := time.Now()
	sent := 0
	// От старых к новым, чтобы в чате сообщения шли по порядку.
	for i := len(listings) - 1; i >= 0; i-- {
		l := listings[i]
		if w.state.IsSeen(c.Name, l.ID) {
			continue
		}
		if reason := c.rejectReason(l, now); reason != "" {
			if w.debug {
				logf("[%s] пропуск «%s»: %s", c.Name, l.Title, reason)
			}
			w.state.MarkSeen(c.Name, l.ID)
			continue
		}
		// Лишнее не теряется: не помечено как виденное — уйдёт в следующем цикле.
		if sent >= w.cfg.MaxNotificationsPerCycle {
			logf("[%s] за цикл отправлено %d, остальные подходящие уйдут в следующий проход", c.Name, sent)
			return
		}
		if w.deliver(ctx, c, l) {
			sent++
			w.stats.sent++
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// deliver отправляет объявление. Виденным оно помечается только после
// успеха — сбой Telegram не должен навсегда терять объявление. Но после
// maxSendFailures неудач подряд оно пропускается, чтобы не долбить вечно.
func (w *Watcher) deliver(ctx context.Context, c *Category, l *Listing) bool {
	var photo []byte
	if w.cfg.sendPhoto && l.Photo != "" {
		if err := sleepCtx(ctx, randDuration(w.cfg.photoDelay)); err != nil {
			return false
		}
		photo = w.avito.FetchPhoto(ctx, l.Photo)
	}
	label := ""
	if c.TopicID == 0 && len(w.cats) > 1 {
		label = c.Name // без темы раздел видно только по подписи
	}
	text := formatListing(l, label, w.cfg.showTime, time.Now())

	dst := Destination{ChatID: c.ChatID, ThreadID: c.TopicID}
	msgID, err := w.tg.SendListing(ctx, dst, text, photo)
	if err != nil && IsThreadError(err) && ctx.Err() == nil {
		dst.ThreadID = w.recoverTopic(ctx, c)
		msgID, err = w.tg.SendListing(ctx, dst, text, photo)
	}

	key := c.Name + "|" + l.ID
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		w.failures[key]++
		if w.failures[key] >= maxSendFailures {
			logf("[%s] «%s» не отправилось %d раза подряд (%v) — пропускаю", c.Name, l.Title, w.failures[key], err)
			delete(w.failures, key)
			w.state.MarkSeen(c.Name, l.ID)
		} else {
			logf("[%s] не удалось отправить «%s»: %v — повторю в следующем цикле", c.Name, l.Title, err)
		}
		return false
	}
	delete(w.failures, key)
	w.state.MarkSeen(c.Name, l.ID)
	if msgID != 0 {
		w.state.AddSent(SentMessage{
			ChatID:    dst.ChatID,
			MessageID: msgID,
			SentAt:    time.Now().Unix(),
			Category:  c.Name,
			ListingID: l.ID,
		})
	}
	logf("[%s] отправлено: %s — %s", c.Name, l.Title, priceText(l.Price))
	return true
}

func (w *Watcher) afterCycle(ctx context.Context) {
	w.deleteOld(ctx)
	if w.state.dirty {
		if err := w.state.Save(); err != nil {
			logf("Не удалось сохранить состояние: %v", err)
		}
	}
	// Cookies стареют, и «сожжённая» сессия продолжает ловить 429,
	// а свежая работает. Поэтому обновляем её и по времени.
	if ttl := w.cfg.sessionTTL; ttl > 0 && time.Since(w.avito.born) >= ttl {
		logf("Сессии больше %s — обновляю cookies.", humanDuration(ttl))
		w.newSession(ctx, true)
	}
	if time.Since(w.lastReport) >= time.Hour {
		s := w.stats
		share := 0.0
		if s.requests > 0 {
			share = 100 * float64(max(0, s.requests-s.blocked-s.failed)) / float64(s.requests)
		}
		logf("За час: запросов к выдаче %d (удачных %.0f%%), блокировок %d, прочих сбоев %d, "+
			"объявлений просмотрено %d, отправлено %d, интервал %s",
			s.requests, share, s.blocked, s.failed, s.listings, s.sent, w.interval.Round(time.Second))
		w.stats = counters{}
		w.lastReport = time.Now()
	}
}

// deleteOld удаляет сообщения бота старше delete_after_hours. Те, что
// Telegram удалить уже не даст, просто забываются.
func (w *Watcher) deleteOld(ctx context.Context) {
	if w.cfg.deleteAfter <= 0 || len(w.state.Sent) == 0 {
		return
	}
	now := time.Now()
	keep := make([]SentMessage, 0, len(w.state.Sent))
	removed, expired := 0, 0
	for _, m := range w.state.Sent {
		age := now.Sub(time.Unix(m.SentAt, 0))
		if age < w.cfg.deleteAfter || ctx.Err() != nil {
			keep = append(keep, m)
			continue
		}
		if age >= 48*time.Hour {
			expired++ // окно упущено, Telegram всё равно откажет
			continue
		}
		err := w.tg.DeleteMessage(ctx, m.ChatID, m.MessageID)
		var te *TelegramError
		switch {
		case err == nil:
			removed++
		case errors.As(err, &te) && te.Code != 429:
			// 400: сообщение уже удалено вручную или недоступно — забываем о нём
			logf("Сообщение %d не удалено (%v), убираю из списка", m.MessageID, err)
		default:
			keep = append(keep, m) // сеть или 429 — попробуем в следующем цикле
		}
	}
	if len(keep) != len(w.state.Sent) {
		w.state.Sent = keep
		w.state.dirty = true
	}
	if removed > 0 || expired > 0 {
		logf("Удалено старых сообщений: %d, упущено окно 48 ч: %d", removed, expired)
	}
}

func (w *Watcher) handleFetchError(ctx context.Context, err error) time.Duration {
	penalty := w.backoff
	var rl *RateLimitError
	switch {
	case errors.As(err, &rl):
		w.stats.blocked++
		w.consecutiveBlocks++
		if rl.RetryAfter > 0 {
			penalty = rl.RetryAfter
		}
		logf("%v. Пауза %s.", err, penalty)
		if !w.switchProxy(ctx, "Лимит запросов.") && w.consecutiveBlocks%2 == 0 {
			logf("Два отказа подряд — беру свежую сессию.")
			w.newSession(ctx, false)
		}
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrBlockPage):
		w.stats.blocked++
		w.consecutiveBlocks++
		logf("%v. Пауза %s.", err, penalty)
		// Пересборка с прогревом — ещё один запрос ровно тогда, когда нас
		// ограничивают, поэтому не чаще раза в три отказа.
		if !w.switchProxy(ctx, "Блокировка.") && w.consecutiveBlocks%3 == 0 {
			logf("Три отказа подряд — пересобираю сессию.")
			w.newSession(ctx, true)
		}
	case errors.Is(err, ErrNoItems):
		w.stats.failed++
		logf("ВНИМАНИЕ: %v. Пауза %s.", err, penalty)
	default:
		w.stats.failed++
		logf("Ошибка запроса: %v. Пауза %s.", err, penalty)
		w.switchProxy(ctx, "Ошибка соединения.")
	}
	w.backoff = min(w.backoff*2, maxBackoff)
	return penalty
}

// recordOutcome подстраивает интервал по доле удачных циклов. Заблокированный
// запрос не ускоряет, а замедляет: цель — не минимальный интервал, а
// максимальная доля успешных проходов.
func (w *Watcher) recordOutcome(ok bool) {
	if !w.cfg.adaptive {
		return
	}
	w.recent = append(w.recent, ok)
	if len(w.recent) > 10 {
		w.recent = w.recent[len(w.recent)-10:]
	}
	if len(w.recent) < 4 {
		return
	}
	good := 0
	for _, r := range w.recent {
		if r {
			good++
		}
	}
	rate := float64(good) / float64(len(w.recent))
	target := w.cfg.TargetSuccessRate
	old := w.interval
	switch {
	case rate < target-0.15:
		w.interval = time.Duration(float64(w.interval)*1.4) + 5*time.Second
	case rate < target:
		w.interval = time.Duration(float64(w.interval)*1.15) + 2*time.Second
	case rate > 0.95:
		w.interval = time.Duration(float64(w.interval) * 0.85)
	}
	w.interval = clampDuration(w.interval, w.minInterval, w.maxInterval)
	if w.interval.Round(time.Second) != old.Round(time.Second) {
		logf("Удачных циклов %.0f%% из последних %d — интервал %s → %s",
			rate*100, len(w.recent), old.Round(time.Second), w.interval.Round(time.Second))
	}
}

// jitteredInterval: ±20%, чтобы запросы не шли строго по таймеру.
func (w *Watcher) jitteredInterval() time.Duration {
	d := time.Duration(float64(w.interval) * (0.8 + 0.4*rand.Float64()))
	return max(d, w.minInterval)
}

func (w *Watcher) currentProxy() *url.URL {
	if len(w.proxies) == 0 {
		return nil
	}
	return w.proxies[w.proxyIdx]
}

func (w *Watcher) switchProxy(ctx context.Context, reason string) bool {
	if len(w.proxies) <= 1 {
		return false
	}
	w.proxyIdx = (w.proxyIdx + 1) % len(w.proxies)
	logf("%s Переключаюсь на прокси №%d из %d.", reason, w.proxyIdx+1, len(w.proxies))
	w.newSession(ctx, true)
	w.avito.CheckExitIP(ctx)
	return true
}

func (w *Watcher) newSession(ctx context.Context, warm bool) {
	if w.avito != nil {
		w.avito.Close()
	}
	w.avito = newAvitoClient(w.currentProxy(), w.debug)
	if warm {
		w.avito.Warm(ctx)
	}
}

// shortSearch — короткое имя поиска для лога: значение q или конец пути.
func shortSearch(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if q := u.Query().Get("q"); q != "" {
		return "«" + q + "»"
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	return parts[len(parts)-1]
}
