package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// parseProxyLine понимает те же форматы, что и Python-версия:
//
//	IP:порт:логин:пароль             (по умолчанию socks5)
//	socks5://IP:порт:логин:пароль    (явный протокол)
//	http://логин:пароль@IP:порт
//	IP:порт                          (без авторизации)
//
// Пустые строки и строки с # в начале пропускаются (nil, nil).
func parseProxyLine(line string) (*url.URL, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, nil
	}
	scheme := "socks5"
	if i := strings.Index(line, "://"); i >= 0 {
		scheme, line = strings.ToLower(line[:i]), line[i+3:]
	}
	// socks5h в Python означал «имена резолвит прокси». Go-шный SOCKS5 так
	// работает всегда, а схему socks5h знают не все версии Go.
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	switch scheme {
	case "socks5", "http", "https":
	default:
		return nil, fmt.Errorf("неизвестный протокол прокси %q", scheme)
	}
	// Сообщения об ошибках намеренно без самой строки: в ней пароль.
	if strings.Contains(line, "@") {
		u, err := url.Parse(scheme + "://" + line)
		if err != nil || u.Host == "" {
			return nil, errors.New("не удалось разобрать адрес вида логин:пароль@IP:порт")
		}
		return u, nil
	}
	parts := strings.Split(line, ":")
	switch len(parts) {
	case 2:
		return &url.URL{Scheme: scheme, Host: net.JoinHostPort(parts[0], parts[1])}, nil
	case 4:
		return &url.URL{
			Scheme: scheme,
			Host:   net.JoinHostPort(parts[0], parts[1]),
			User:   url.UserPassword(parts[2], parts[3]),
		}, nil
	}
	return nil, errors.New("непонятный формат (ожидается IP:порт или IP:порт:логин:пароль)")
}

// loadProxies собирает пул из config.proxies и файла proxies.txt.
// Пустой пул — работа напрямую.
func loadProxies(cfg *Config, baseDir string) ([]*url.URL, error) {
	lines := append([]string(nil), cfg.Proxies...)
	path := cfg.ProxiesFile
	if path == "" {
		path = "proxies.txt"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		lines = append(lines, strings.Split(string(data), "\n")...)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	var out []*url.URL
	for n, line := range lines {
		u, err := parseProxyLine(line)
		if err != nil {
			logf("Прокси №%d пропущен: %v", n+1, err)
			continue
		}
		if u != nil {
			out = append(out, u)
		}
	}
	return out, nil
}
