package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// Listing — одно объявление из выдачи.
type Listing struct {
	ID          string
	Title       string
	Price       *int // nil — цена не указана
	URL         string
	City        string
	Description string
	Photo       string
	Published   time.Time // нулевое значение — время неизвестно
	Approx      bool      // время восстановлено из «N часов назад», точность ±1 ед.
}

type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("Авито ограничил частоту запросов (HTTP %d)", e.Status)
}

var (
	ErrForbidden = errors.New("Авито ответил 403: запрос сочтён подозрительным")
	ErrBlockPage = errors.New("Авито показал страницу блокировки вместо выдачи")
	ErrNoItems   = errors.New("в выдаче ни одного объявления: заглушка антибота или новая вёрстка (запустите с -debug)")
)

// Согласованные наборы заголовков: User-Agent и sec-ch-ua должны описывать
// одну и ту же версию браузера. Версии стоит обновлять раз в несколько месяцев.
type browserProfile struct{ UA, ChUA, Platform string }

var browserProfiles = []browserProfile{
	{
		UA:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
		ChUA:     `"Chromium";v="138", "Google Chrome";v="138", "Not-A.Brand";v="99"`,
		Platform: `"Windows"`,
	},
	{
		UA:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
		ChUA:     `"Chromium";v="137", "Google Chrome";v="137", "Not-A.Brand";v="99"`,
		Platform: `"Windows"`,
	},
	{
		UA:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
		ChUA:     `"Chromium";v="138", "Google Chrome";v="138", "Not-A.Brand";v="99"`,
		Platform: `"macOS"`,
	},
}

var avitoRoot = &url.URL{Scheme: "https", Host: "www.avito.ru", Path: "/"}

// AvitoClient — одна «сессия»: свой набор cookies, свой профиль браузера,
// свой прокси. Пересоздаётся по таймеру и после отказов.
type AvitoClient struct {
	client   *http.Client
	proxyURL *url.URL
	profile  browserProfile
	born     time.Time
	debug    bool
}

func newAvitoClient(proxy *url.URL, debug bool) *AvitoClient {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(proxy), // nil — напрямую
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	return &AvitoClient{
		client:   &http.Client{Transport: tr, Jar: jar, Timeout: 30 * time.Second},
		proxyURL: proxy,
		profile:  browserProfiles[rand.Intn(len(browserProfiles))],
		born:     time.Now(),
		debug:    debug,
	}
}

func (a *AvitoClient) Close() { a.client.CloseIdleConnections() }

func (a *AvitoClient) newRequest(ctx context.Context, rawURL, kind string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("User-Agent", a.profile.UA)
	h.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	h.Set("sec-ch-ua", a.profile.ChUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", a.profile.Platform)
	// Accept-Encoding намеренно не ставим: тогда Go сам запросит gzip
	// и сам распакует ответ.
	switch kind {
	case "image":
		h.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
		h.Set("Referer", "https://www.avito.ru/")
		h.Set("Sec-Fetch-Dest", "image")
		h.Set("Sec-Fetch-Mode", "no-cors")
		h.Set("Sec-Fetch-Site", "cross-site")
	default:
		h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
		h.Set("Upgrade-Insecure-Requests", "1")
		h.Set("Sec-Fetch-Dest", "document")
		h.Set("Sec-Fetch-Mode", "navigate")
		h.Set("Sec-Fetch-User", "?1")
		if kind == "home" {
			h.Set("Sec-Fetch-Site", "none")
		} else {
			h.Set("Referer", "https://www.avito.ru/")
			h.Set("Sec-Fetch-Site", "same-origin")
		}
	}
	return req, nil
}

func (a *AvitoClient) get(ctx context.Context, rawURL, kind string, limit int64) (*http.Response, []byte, error) {
	req, err := a.newRequest(ctx, rawURL, kind)
	if err != nil {
		return nil, nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp, body, err
}

// Warm — одна загрузка главной ради cookies. Каталог не грузим: лимит
// у Авито стоит именно на частоту обращений, лишний запрос только вредит.
func (a *AvitoClient) Warm(ctx context.Context) {
	resp, _, err := a.get(ctx, avitoRoot.String(), "home", 8<<20)
	if err != nil {
		if ctx.Err() == nil {
			logf("Прогрев не удался: %v", err)
		}
		return
	}
	if resp.StatusCode == http.StatusOK {
		logf("Прогрев: главная открылась, cookies получено: %d", len(a.client.Jar.Cookies(avitoRoot)))
	} else {
		logf("Прогрев: главная ответила %d — похоже, блокируется весь адрес, а не только выдача", resp.StatusCode)
	}
	_ = sleepCtx(ctx, randDuration(Range{1.5, 3.5}))
}

// CheckExitIP пишет в лог, с какого адреса и из какой страны идут запросы.
// ip-api.com на бесплатном тарифе отвечает только по http.
func (a *AvitoClient) CheckExitIP(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://ip-api.com/json/?fields=status,message,country,countryCode,hosting,query", nil)
	if err != nil {
		return
	}
	resp, err := a.client.Do(req)
	if err != nil {
		logf("Не удалось проверить выходной IP: %v", err)
		return
	}
	defer resp.Body.Close()
	var d struct {
		Status      string `json:"status"`
		Message     string `json:"message"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		Hosting     bool   `json:"hosting"`
		Query       string `json:"query"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&d); err != nil || d.Status != "success" {
		logf("Не удалось проверить выходной IP %s", d.Message)
		return
	}
	logf("Выходной IP: %s | страна: %s | датацентр: %v", d.Query, d.Country, d.Hosting)
	if a.proxyURL != nil && d.CountryCode != "RU" {
		logf("ВНИМАНИЕ: выход не из России — Авито может блокировать. Проверьте прокси.")
	}
	if a.proxyURL == nil && d.Hosting {
		logf("ВНИМАНИЕ: запросы идут с адреса дата-центра. Авито такие диапазоны блокирует жёстко — 403 и 429 здесь обычное дело.")
	}
}

// FetchSearch загружает страницу выдачи и разбирает объявления.
// Пустой список без ошибки — честно пустая выдача; решение, считать ли это
// сбоем, принимает вызывающий код.
func (a *AvitoClient) FetchSearch(ctx context.Context, rawURL string) ([]*Listing, error) {
	resp, body, err := a.get(ctx, rawURL, "page", 20<<20)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, 439: // 439 — нестандартный код Авито, смысл тот же
		return nil, &RateLimitError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case http.StatusForbidden:
		a.saveDebug(body)
		return nil, ErrForbidden
	default:
		return nil, fmt.Errorf("Авито ответил HTTP %d", resp.StatusCode)
	}
	a.saveDebug(body)
	items := parseSearchPage(body, time.Now())
	if len(items) == 0 && looksBlocked(body) {
		return nil, ErrBlockPage
	}
	return items, nil
}

// FetchPhoto скачивает картинку сам: по ссылке Telegram её у Авито
// не получает, а загруженный файл такой проблемы не имеет.
func (a *AvitoClient) FetchPhoto(ctx context.Context, rawURL string) []byte {
	const limit = 9 << 20 // у Telegram предел 10 МБ на фото
	resp, body, err := a.get(ctx, rawURL, "image", limit+1)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			logf("Картинка не скачалась: %v", err)
		}
		return nil
	case resp.StatusCode != http.StatusOK:
		logf("Картинка не скачалась (HTTP %d)", resp.StatusCode)
		return nil
	case len(body) == 0 || len(body) > limit:
		return nil
	}
	return body
}

func (a *AvitoClient) saveDebug(body []byte) {
	if !a.debug {
		return
	}
	if err := os.WriteFile("debug_last_page.html", body, 0o644); err != nil {
		logf("Не удалось сохранить debug_last_page.html: %v", err)
	}
}

func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func looksBlocked(body []byte) bool {
	low := bytes.ToLower(body)
	return bytes.Contains(body, []byte("Доступ ограничен")) ||
		bytes.Contains(low, []byte("firewall")) ||
		bytes.Contains(low, []byte("captcha"))
}

// ---------------------------------------------------------------------------
// Разбор страницы выдачи.
//
// Основной источник — JSON-состояние, которое Авито кладёт в страницу
// (<script data-mfe-state>): там у объявления точная метка времени, а не
// округлённое «2 часа назад». Если JSON не нашёлся или поменялся, работает
// запасной разбор по вёрстке (data-marker), как в Python-версии. Пропуски
// в JSON дополняются данными из вёрстки.
// ---------------------------------------------------------------------------

var (
	reStateScript = regexp.MustCompile(`(?s)<script[^>]*data-mfe-state[^>]*>(.*?)</script>`)
	rePrice       = regexp.MustCompile(`\d[\d\s\x{00a0}\x{2009}\x{202f}]*`)
	reKeySize     = regexp.MustCompile(`(\d{2,4})x(\d{2,4})`)
	reURLSize     = regexp.MustCompile(`/(\d{3,4})x(\d{3,4})[/.]`)
	reAgo         = regexp.MustCompile(`(\d+)\s*(секунд|минут|час|день|дн|недел|месяц)`)
	reClock       = regexp.MustCompile(`(\d{1,2}):(\d{2})`)
	reDayMonth    = regexp.MustCompile(`(\d{1,2})\s+(январ|феврал|март|апрел|ма[йя]|июн|июл|август|сентябр|октябр|ноябр|декабр)`)
)

func parseSearchPage(body []byte, now time.Time) []*Listing {
	items := parseStateJSON(body, now)
	fromHTML := parseHTML(body, now)
	if len(items) == 0 {
		return fromHTML
	}
	byID := make(map[string]*Listing, len(fromHTML))
	for _, h := range fromHTML {
		byID[h.ID] = h
	}
	for _, l := range items {
		h := byID[l.ID]
		if h == nil {
			continue
		}
		if l.Published.IsZero() {
			l.Published, l.Approx = h.Published, h.Approx
		}
		if l.Title == "" {
			l.Title = h.Title
		}
		if l.Price == nil {
			l.Price = h.Price
		}
		if l.Photo == "" {
			l.Photo = h.Photo
		}
		if l.City == "" {
			l.City = h.City
		}
		if l.Description == "" {
			l.Description = h.Description
		}
	}
	return items
}

func parseStateJSON(body []byte, now time.Time) []*Listing {
	var out []*Listing
	seen := make(map[string]bool)
	for _, m := range reStateScript.FindAllSubmatch(body, -1) {
		raw := bytes.TrimSpace(m[1])
		if !bytes.Contains(raw, []byte("urlPath")) {
			continue
		}
		root, ok := decodeJSON(raw)
		if !ok { // содержимое бывает экранировано как HTML
			root, ok = decodeJSON([]byte(html.UnescapeString(string(raw))))
		}
		if !ok {
			continue
		}
		var objs []map[string]any
		collectItems(root, 0, &objs)
		for _, obj := range objs {
			l := listingFromJSON(obj, now)
			if seen[l.ID] {
				continue
			}
			seen[l.ID] = true
			out = append(out, l)
		}
	}
	return out
}

func decodeJSON(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // id объявления — 10+ цифр, float64 их не испортит, но так надёжнее
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// collectItems обходит JSON и собирает всё, что похоже на объявление.
// Путь к списку в состоянии страницы меняется от релиза к релизу, поэтому
// ищем не по пути, а по форме объекта.
func collectItems(v any, depth int, out *[]map[string]any) {
	if depth > 60 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if looksLikeItem(t) {
			*out = append(*out, t)
			return
		}
		for _, child := range t {
			collectItems(child, depth+1, out)
		}
	case []any:
		for _, child := range t {
			collectItems(child, depth+1, out)
		}
	}
}

// looksLikeItem: числовой id, заголовок и адрес вида «…/psp_3008_<id>».
// Последнее отсекает рубрики и хлебные крошки, у которых тоже есть id и title.
func looksLikeItem(m map[string]any) bool {
	id := jsonID(m["id"])
	if len(id) < 6 || !isDigits(id) {
		return false
	}
	title, _ := m["title"].(string)
	urlPath, _ := m["urlPath"].(string)
	return title != "" && strings.Contains(urlPath, "_"+id)
}

func listingFromJSON(m map[string]any, now time.Time) *Listing {
	id := jsonID(m["id"])
	title, _ := m["title"].(string)
	l := &Listing{
		ID:    id,
		Title: cleanSpaces(title),
		// Короткая ссылка: одинаковая у всех получателей, без меток сессии.
		URL:   "https://www.avito.ru/" + id,
		Price: jsonPrice(m),
		Photo: biggestImage(m["images"]),
	}
	if ts, ok := jsonInt(m["sortTimeStamp"]); ok && ts > 0 {
		var t time.Time
		if ts > 1e12 {
			t = time.UnixMilli(ts)
		} else {
			t = time.Unix(ts, 0)
		}
		// Защита от мусора: из будущего (с запасом на расхождение часов) и древность.
		if t.Before(now.Add(10*time.Minute)) && t.After(now.AddDate(-5, 0, 0)) {
			l.Published = t
		}
	}
	if loc, ok := m["location"].(map[string]any); ok {
		l.City, _ = loc["name"].(string)
	}
	if l.City == "" {
		if geo, ok := m["geo"].(map[string]any); ok {
			l.City, _ = geo["formattedAddress"].(string)
		}
	}
	l.City = truncateRunes(cleanSpaces(l.City), 80)
	if d, ok := m["description"].(string); ok {
		l.Description = truncateRunes(cleanSpaces(d), 400)
	}
	return l
}

func jsonID(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func jsonInt(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(t), true
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

func jsonPrice(m map[string]any) *int {
	if pd, ok := m["priceDetailed"].(map[string]any); ok {
		s, _ := pd["string"].(string)
		if strings.Contains(strings.ToLower(s), "бесплатно") {
			zero := 0
			return &zero
		}
		if v, ok := jsonInt(pd["value"]); ok && v > 0 {
			p := int(v)
			return &p
		}
		if p := parsePrice(s); p != nil {
			return p
		}
	}
	if v, ok := jsonInt(m["price"]); ok && v > 0 {
		p := int(v)
		return &p
	}
	return nil
}

// biggestImage: images — список словарей «размер → адрес», берём самый крупный.
func biggestImage(v any) string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	sizes, ok := arr[0].(map[string]any)
	if !ok {
		return ""
	}
	best, bestArea := "", -1
	for key, val := range sizes {
		u, _ := val.(string)
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		}
		if !strings.HasPrefix(u, "http") {
			continue
		}
		area := 0
		if m := reKeySize.FindStringSubmatch(key); m != nil {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			area = w * h
		}
		if area > bestArea {
			best, bestArea = u, area
		}
	}
	return best
}

// parseHTML — запасной разбор по вёрстке карточек.
func parseHTML(body []byte, now time.Time) []*Listing {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var out []*Listing
	doc.Find(`[data-marker="item"]`).Each(func(_ int, item *goquery.Selection) {
		id := item.AttrOr("data-item-id", "")
		if id == "" {
			id = strings.TrimPrefix(item.AttrOr("id", ""), "i")
		}
		if !isDigits(id) {
			return
		}
		l := &Listing{ID: id, URL: "https://www.avito.ru/" + id}

		title := item.Find(`[data-marker="item-title"]`).First()
		if title.Length() == 0 {
			title = item.Find(`a[itemprop="url"]`).First()
		}
		l.Title = cleanSpaces(title.Text())
		if l.Title == "" {
			l.Title = cleanSpaces(title.AttrOr("title", ""))
		}

		// Видимая цена карточки; meta внутри неё — точное число без пробелов.
		if pe := item.Find(`[data-marker="item-price"]`).First(); pe.Length() > 0 {
			if v, ok := pe.Find(`meta[itemprop="price"]`).Attr("content"); ok {
				l.Price = parsePrice(v)
			}
			if l.Price == nil {
				l.Price = parsePrice(pe.Text())
			}
		}

		if dt := cleanSpaces(item.Find(`[data-marker="item-date"]`).First().Text()); dt != "" {
			l.Published, l.Approx = parseAvitoDate(dt, now)
		}

		for _, sel := range []string{`[data-marker="item-description"]`, `[class*="iva-item-descriptionStep"]`, `[class*="item-description"]`} {
			if d := cleanSpaces(item.Find(sel).First().Text()); d != "" {
				l.Description = truncateRunes(d, 400)
				break
			}
		}

		for _, sel := range []string{`[data-marker="item-location"]`, `[data-marker="item-address"]`, `[class*="geo-root"]`, `[class*="geo-georeferences"]`} {
			if c := cleanSpaces(item.Find(sel).First().Text()); c != "" {
				l.City = truncateRunes(c, 80)
				break
			}
		}
		if l.City == "" {
			// Блок города в выдаче часто пустой (его дорисовывает скрипт),
			// зато город есть в alt картинки: «PSP 3008, Тихорецк».
			item.Find("img").EachWithBreak(func(_ int, img *goquery.Selection) bool {
				alt := strings.TrimSpace(img.AttrOr("alt", ""))
				if i := strings.LastIndex(alt, ","); i >= 0 {
					tail := strings.TrimSpace(alt[i+1:])
					if tail != "" && utf8.RuneCountInString(tail) <= 40 && !isDigits(tail) {
						l.City = tail
						return false
					}
				}
				return true
			})
		}

		l.Photo = bestPhoto(item)
		out = append(out, l)
	})
	return out
}

// bestPhoto собирает адреса картинок из слайдера, src и srcset
// и выбирает самую крупную.
func bestPhoto(item *goquery.Selection) string {
	best, bestScore := "", -1
	consider := func(src string, bonus int) {
		src = strings.TrimSpace(src)
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		if !strings.HasPrefix(src, "http") || !strings.Contains(src, "avito.st") {
			return
		}
		score := bonus
		if m := reURLSize.FindStringSubmatch(src); m != nil {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			score += w * h * 10
		}
		if score > bestScore {
			best, bestScore = src, score
		}
	}
	item.Find(`[data-marker^="slider-image/image-"]`).Each(func(_ int, s *goquery.Selection) {
		consider(strings.TrimPrefix(s.AttrOr("data-marker", ""), "slider-image/image-"), 0)
	})
	item.Find("img").Each(func(_ int, img *goquery.Selection) {
		for _, attr := range []string{"src", "data-src", "data-original"} {
			consider(img.AttrOr(attr, ""), 1)
		}
		for _, part := range strings.Split(img.AttrOr("srcset", ""), ",") {
			fields := strings.Fields(part)
			if len(fields) == 0 {
				continue
			}
			bonus := 2
			if len(fields) > 1 { // «url 2x» или «url 472w» — вариант покрупнее
				if f, err := strconv.ParseFloat(strings.TrimRight(fields[1], "xw"), 64); err == nil {
					bonus += int(f)
				}
			}
			consider(fields[0], bonus)
		}
	})
	return best
}

// parsePrice берёт первое число («5 000 ₽» → 5000). Раньше склеивались все
// цифры блока, и «5 000 ₽ 6 500 ₽» превращалось бы в 50006500.
func parsePrice(s string) *int {
	m := rePrice.FindString(s)
	if m == "" {
		return nil
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, m)
	v, err := strconv.Atoi(digits)
	if err != nil {
		return nil
	}
	return &v
}

// parseAvitoDate переводит подпись даты из вёрстки во время. Понимает
// «N минут/часов/дней назад», «сегодня в 14:30», «вчера в 09:10»,
// «3 октября 14:20». Второе значение — время приблизительное
// («2 часа назад» на деле означает от 2:00 до 2:59).
func parseAvitoDate(text string, now time.Time) (time.Time, bool) {
	t := normalizeText(text)
	switch {
	case t == "":
		return time.Time{}, false
	case strings.Contains(t, "только что"):
		return now, false
	}
	if m := reAgo.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit, approx := time.Minute, false
		switch m[2] {
		case "секунд":
			unit = time.Second
		case "час":
			unit, approx = time.Hour, true
		case "день", "дн":
			unit, approx = 24*time.Hour, true
		case "недел":
			unit, approx = 7*24*time.Hour, true
		case "месяц":
			unit, approx = 30*24*time.Hour, true
		}
		return now.Add(-time.Duration(n) * unit), approx
	}
	if strings.Contains(t, "секунд") { // «несколько секунд назад»
		return now, false
	}

	msk := now.In(moscow)
	var day time.Time
	switch {
	case strings.Contains(t, "сегодня"):
		day = msk
	case strings.Contains(t, "вчера"):
		day = msk.AddDate(0, 0, -1)
	default:
		m := reDayMonth.FindStringSubmatch(t)
		if m == nil {
			return time.Time{}, false
		}
		d, _ := strconv.Atoi(m[1])
		day = time.Date(msk.Year(), monthIndex(m[2]), d, 0, 0, 0, 0, moscow)
		if day.After(msk) { // «31 декабря» в начале января — прошлый год
			day = day.AddDate(-1, 0, 0)
		}
	}
	if c := reClock.FindStringSubmatch(t); c != nil {
		hh, _ := strconv.Atoi(c[1])
		mm, _ := strconv.Atoi(c[2])
		return time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, moscow), false
	}
	if strings.Contains(t, "сегодня") {
		return time.Time{}, false // «сегодня» без часов — точнее не сказать
	}
	// Дата без времени: берём конец того дня — самую свежую из возможных оценок.
	return time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 0, 0, moscow), true
}

func monthIndex(s string) time.Month {
	prefixes := []string{"январ", "феврал", "март", "апрел", "ма", "июн", "июл", "август", "сентябр", "октябр", "ноябр", "декабр"}
	for i, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return time.Month(i + 1)
		}
	}
	return time.January
}
