package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Matcher — одно слово или фраза из белого/чёрного списка.
//
// Обычная запись ищется с начала слова: «плата» найдёт «плата» и «платы»,
// но не «оплата» и не «предоплата»; «зарядк» найдёт «зарядка», «зарядкой».
// Регистр и ё/е не важны. Запись вида "re:выражение" — регулярное
// выражение RE2 (внимание: \b в Go понимает только латиницу).
type Matcher struct {
	Raw  string
	word string
	re   *regexp.Regexp
}

func (m Matcher) Match(text string) bool {
	if m.re != nil {
		return m.re.MatchString(text)
	}
	return containsAtWordStart(text, m.word)
}

// normalizeText: нижний регистр, ё→е, любые пробелы (включая неразрывные)
// схлопнуты в один обычный.
func normalizeText(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "ё", "е")
	return strings.Join(strings.Fields(s), " ")
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func containsAtWordStart(text, word string) bool {
	if word == "" {
		return false
	}
	// Фраза начинается не с буквы («+ игры») — границу слова не проверяем.
	if first, _ := utf8.DecodeRuneInString(word); !isWordRune(first) {
		return strings.Contains(text, word)
	}
	for from := 0; from <= len(text); {
		i := strings.Index(text[from:], word)
		if i < 0 {
			return false
		}
		pos := from + i
		if pos == 0 {
			return true
		}
		if prev, _ := utf8.DecodeLastRuneInString(text[:pos]); !isWordRune(prev) {
			return true
		}
		from = pos + 1
	}
	return false
}

func buildMatchers(words []string, label string) ([]Matcher, error) {
	var out []Matcher
	for _, raw := range words {
		w := strings.TrimSpace(raw)
		if w == "" {
			continue
		}
		if expr, ok := strings.CutPrefix(w, "re:"); ok {
			re, err := regexp.Compile("(?i)" + expr)
			if err != nil {
				return nil, fmt.Errorf("%s: неверное выражение %q: %v", label, w, err)
			}
			out = append(out, Matcher{Raw: w, re: re})
			continue
		}
		warnMixedScript(w, label)
		out = append(out, Matcher{Raw: w, word: normalizeText(w)})
	}
	return out, nil
}

// warnMixedScript ловит слова, где перемешаны кириллица и латиница
// (А, Р, С, О, Е выглядят одинаково). Такое слово не совпадёт никогда.
// Фразу «для psp» проверяем по словам: смешение между словами законно.
func warnMixedScript(w, label string) {
	tokens := strings.FieldsFunc(w, func(r rune) bool { return !isWordRune(r) })
	for _, tok := range tokens {
		var cyr, lat bool
		for _, r := range tok {
			switch {
			case unicode.Is(unicode.Cyrillic, r):
				cyr = true
			case r < 128 && unicode.IsLetter(r):
				lat = true
			}
		}
		if cyr && lat {
			logf("%s: в слове «%s» смешаны кириллица и латиница — скорее всего опечатка, совпадений не будет", label, tok)
		}
	}
}

func firstMatch(ms []Matcher, text string) (string, bool) {
	for _, m := range ms {
		if m.Match(text) {
			return m.Raw, true
		}
	}
	return "", false
}

// rejectReason возвращает причину отсева или пустую строку, если объявление
// подходит. Все проверки независимы и идут по очереди.
func (c *Category) rejectReason(l *Listing, now time.Time) string {
	title := normalizeText(l.Title)
	desc := normalizeText(l.Description)

	// Чёрный список — только по заголовку. В описаниях нормальных объявлений
	// постоянно встречаются «зарядка», «чехол», «карта памяти», «батарея
	// держит» — проверка по описанию отсеивала бы как раз полные комплекты.
	if w, ok := firstMatch(c.Exclude, title); ok {
		return fmt.Sprintf("в заголовке «%s»", w)
	}
	// Отдельный короткий список для признаков клонов («встроенных игр»):
	// они часто прячутся именно в описании.
	if w, ok := firstMatch(c.DescExclude, title+" "+desc); ok {
		return fmt.Sprintf("в тексте «%s»", w)
	}
	if len(c.Include) > 0 {
		hay := title
		if c.IncludeInDescription {
			hay = title + " " + desc
		}
		if _, ok := firstMatch(c.Include, hay); !ok {
			return "нет ни одного слова из белого списка"
		}
	}

	if l.Price == nil {
		if !c.NotifyWithoutPrice {
			return "цена не указана"
		}
	} else {
		p := *l.Price
		if p < c.MinPrice {
			return fmt.Sprintf("цена %s ниже %d", priceText(l.Price), c.MinPrice)
		}
		if c.MaxPrice > 0 && p > c.MaxPrice {
			return fmt.Sprintf("цена %s выше %d", priceText(l.Price), c.MaxPrice)
		}
	}

	if c.MaxAge > 0 {
		if l.Published.IsZero() {
			if !c.NotifyWithoutAge {
				return "время публикации неизвестно"
			}
		} else if age := now.Sub(l.Published); age > c.MaxAge {
			return fmt.Sprintf("опубликовано %s назад", humanDuration(age))
		}
	}
	return ""
}
