// Avito → Telegram watcher.
//
// Следит за выдачей Авито и присылает новые объявления в темы (разделы)
// супергруппы Telegram: каждый раздел конфига — своя тема.
//
// Запуск:
//
//	avito-watcher                  обычный режим
//	avito-watcher -once            один проход по всем разделам и выход
//	avito-watcher -debug           причины отсева в логе + debug_last_page.html
//	avito-watcher -config путь     другой файл настроек
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	configPath := flag.String("config", "config.json", "файл настроек")
	statePath := flag.String("state", "", "файл состояния (по умолчанию state.json рядом с config.json)")
	once := flag.Bool("once", false, "один проход по всем разделам и выход")
	debug := flag.Bool("debug", false, "причины отсева в логе и сохранение страницы выдачи в debug_last_page.html")
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime)

	baseDir := filepath.Dir(*configPath)
	if *statePath == "" {
		*statePath = filepath.Join(baseDir, "state.json")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	cats, err := buildCategories(cfg)
	if err != nil {
		log.Fatal(err)
	}
	proxies, err := loadProxies(cfg, baseDir)
	if err != nil {
		log.Fatal(err)
	}
	tg, err := newTelegram(cfg.TelegramBotToken, cfg.TelegramProxy)
	if err != nil {
		log.Fatal(err)
	}
	st, err := loadState(*statePath)
	if err != nil {
		log.Fatal(err)
	}

	// Ctrl+C и systemctl stop отменяют контекст: все паузы и запросы
	// прерываются сразу, состояние сохраняется ниже.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := newWatcher(cfg, cats, st, tg, proxies, *debug)
	runErr := w.Run(ctx, *once)
	if err := st.Save(); err != nil {
		logf("Не удалось сохранить состояние: %v", err)
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		log.Fatal(runErr)
	}
	logf("Watcher остановлен, состояние сохранено.")
}
