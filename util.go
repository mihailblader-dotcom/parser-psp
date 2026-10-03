package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Авито показывает время по Москве. Москва живёт в UTC+3 без перехода
// на летнее время, поэтому база часовых поясов не нужна.
var moscow = time.FixedZone("MSK", 3*60*60)

func logf(format string, args ...any) { log.Printf(format, args...) }

// sleepCtx — пауза, которую прерывает остановка программы.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// randDuration — случайная пауза из диапазона: запросы по ровному таймеру
// выглядят для антибота слишком по-машинному.
func randDuration(r Range) time.Duration {
	sec := r.Min
	if r.Max > r.Min {
		sec += rand.Float64() * (r.Max - r.Min)
	}
	return time.Duration(sec * float64(time.Second))
}

func clampDuration(d, lo, hi time.Duration) time.Duration { return max(lo, min(d, hi)) }

func cleanSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func humanDuration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%d мин", m)
	case m < 48*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d ч", m/60)
		}
		return fmt.Sprintf("%d ч %d мин", m/60, m%60)
	default:
		return fmt.Sprintf("%d дн", m/(24*60))
	}
}

// formatThousands: 12500 → "12 500".
func formatThousands(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func priceText(p *int) string {
	switch {
	case p == nil:
		return "цена не указана"
	case *p == 0:
		return "бесплатно"
	}
	return formatThousands(*p) + " ₽"
}
