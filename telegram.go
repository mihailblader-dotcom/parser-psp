package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Telegram пускает в одну группу около 20 сообщений в минуту, дальше 429.
const telegramChatGap = 3100 * time.Millisecond

type Telegram struct {
	token    string
	client   *http.Client
	lastSent map[ChatID]time.Time
}

type TelegramError struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("Telegram %d: %s", e.Code, e.Description)
}

// Destination — чат и тема (message_thread_id). ThreadID 0 — общий поток.
type Destination struct {
	ChatID   ChatID
	ThreadID int
}

func newTelegram(token, proxy string) (*Telegram, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, err := parseProxyLine(proxy)
		if err != nil {
			return nil, fmt.Errorf("telegram_proxy: %w", err)
		}
		if u != nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &Telegram{
		token:    token,
		client:   &http.Client{Transport: tr, Timeout: 60 * time.Second},
		lastSent: make(map[ChatID]time.Time),
	}, nil
}

// redact убирает токен из текста ошибки: сетевые ошибки Go содержат полный
// адрес запроса, а в нём /bot<токен>/. Иначе токен утекает в журнал.
func (t *Telegram) redact(err error) error {
	if err == nil || t.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<токен>"))
}

func (t *Telegram) call(ctx context.Context, method string, params map[string]string, photo []byte) (json.RawMessage, error) {
	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	if photo != nil {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range params {
			if err := mw.WriteField(k, v); err != nil {
				return nil, err
			}
		}
		fw, err := mw.CreateFormFile("photo", "photo.jpg")
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(photo); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		body, contentType = &buf, mw.FormDataContentType()
	} else {
		form := url.Values{}
		for k, v := range params {
			form.Set(k, v)
		}
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+t.token+"/"+method, body)
	if err != nil {
		return nil, t.redact(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.redact(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, t.redact(err)
	}

	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("Telegram ответил HTTP %d, ответ не разобран", resp.StatusCode)
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return nil, &TelegramError{
			Code:        code,
			Description: r.Description,
			RetryAfter:  time.Duration(r.Parameters.RetryAfter) * time.Second,
		}
	}
	return r.Result, nil
}

func (t *Telegram) pace(ctx context.Context, chat ChatID) error {
	if wait := telegramChatGap - time.Since(t.lastSent[chat]); wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
	t.lastSent[chat] = time.Now()
	return nil
}

// SendListing отправляет объявление (с фото, если оно есть) в тему чата.
// Возвращает message_id — он нужен, чтобы позже удалить сообщение.
func (t *Telegram) SendListing(ctx context.Context, dst Destination, text string, photo []byte) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := t.pace(ctx, dst.ChatID); err != nil {
			return 0, err
		}
		params := map[string]string{"chat_id": string(dst.ChatID), "parse_mode": "HTML"}
		if dst.ThreadID != 0 {
			params["message_thread_id"] = strconv.Itoa(dst.ThreadID)
		}
		method := "sendMessage"
		if photo != nil {
			method = "sendPhoto"
			params["caption"] = text
		} else {
			params["text"] = text
		}

		res, err := t.call(ctx, method, params, photo)
		if err == nil {
			var msg struct {
				MessageID int `json:"message_id"`
			}
			_ = json.Unmarshal(res, &msg)
			return msg.MessageID, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		var te *TelegramError
		if errors.As(err, &te) {
			switch {
			case te.Code == http.StatusTooManyRequests:
				wait := te.RetryAfter
				if wait <= 0 {
					wait = 5 * time.Second
				}
				logf("Telegram просит подождать %s", wait)
				if err := sleepCtx(ctx, wait+time.Second); err != nil {
					return 0, err
				}
				continue
			case IsThreadError(err):
				return 0, err // тему удалили — решает вызывающий код
			case te.Code == http.StatusBadRequest && photo != nil:
				// Битая картинка — шлём текстом, чтобы объявление не пропало из-за фото.
				logf("Фото не принято (%s), отправляю текстом", te.Description)
				photo = nil
				continue
			case te.Code >= 400 && te.Code < 500:
				return 0, err // повторять бессмысленно, ответ уже всё объяснил
			}
		}
		logf("Telegram: %v (попытка %d из 3)", err, attempt)
		if err := sleepCtx(ctx, time.Duration(2*attempt)*time.Second); err != nil {
			return 0, err
		}
	}
	return 0, lastErr
}

// CreateTopic создаёт тему в супергруппе с включёнными темами. Боту нужны
// права администратора «Управление темами».
func (t *Telegram) CreateTopic(ctx context.Context, chat ChatID, name string) (int, error) {
	res, err := t.call(ctx, "createForumTopic", map[string]string{
		"chat_id": string(chat),
		"name":    truncateRunes(name, 120),
	}, nil)
	if err != nil {
		return 0, err
	}
	var topic struct {
		MessageThreadID int `json:"message_thread_id"`
	}
	if err := json.Unmarshal(res, &topic); err != nil || topic.MessageThreadID == 0 {
		return 0, errors.New("Telegram не вернул номер темы")
	}
	return topic.MessageThreadID, nil
}

func (t *Telegram) DeleteMessage(ctx context.Context, chat ChatID, messageID int) error {
	_, err := t.call(ctx, "deleteMessage", map[string]string{
		"chat_id":    string(chat),
		"message_id": strconv.Itoa(messageID),
	}, nil)
	return err
}

// IsThreadError — тема удалена или не существует.
func IsThreadError(err error) bool {
	var te *TelegramError
	if !errors.As(err, &te) {
		return false
	}
	d := strings.ToLower(te.Description)
	return strings.Contains(d, "thread not found") || strings.Contains(d, "topic_deleted")
}

// formatListing собирает текст уведомления. Всё, что пришло с Авито,
// экранируется: «PSP & игры» или «<прошитая>» в заголовке иначе ломают
// HTML-разметку, и Telegram отвечает 400.
func formatListing(l *Listing, label string, showTime bool, now time.Time) string {
	var b strings.Builder
	if label != "" {
		b.WriteString("🏷 " + html.EscapeString(label) + "\n")
	}
	b.WriteString("🎮 <b>" + html.EscapeString(l.Title) + "</b>\n")
	b.WriteString("💰 " + priceText(l.Price) + "\n")
	if l.City != "" {
		b.WriteString("📍 " + html.EscapeString(l.City) + "\n")
	}
	if showTime && !l.Published.IsZero() {
		b.WriteString("🕒 " + formatPublished(l.Published, now, l.Approx) + "\n")
	}
	b.WriteString("🔗 " + l.URL)
	return b.String()
}

var monthsShort = [...]string{"янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}

func formatPublished(t, now time.Time, approx bool) string {
	t, now = t.In(moscow), now.In(moscow)
	y, m, d := t.Date()
	ny, nm, nd := now.Date()
	yy, ym, yd := now.AddDate(0, 0, -1).Date()
	var day string
	switch {
	case y == ny && m == nm && d == nd:
		day = "сегодня"
	case y == yy && m == ym && d == yd:
		day = "вчера"
	default:
		day = fmt.Sprintf("%d %s", d, monthsShort[m-1])
	}
	prefix := ""
	if approx {
		prefix = "~"
	}
	return fmt.Sprintf("%s%s в %s (МСК)", prefix, day, t.Format("15:04"))
}
