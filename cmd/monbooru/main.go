package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	internalweb "github.com/leqwin/monbooru/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	configPath := flag.String("config", "./monbooru.toml", "path to monbooru.toml config file")
	hashPassword := flag.String("hash-password", "", "print bcrypt hash of the given password and exit")
	flag.Parse()

	if *hashPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*hashPassword), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error hashing password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("FATAL loading config: %v", err)
	}
	logx.Set(cfg.Log.Level)
	logx.Infof("config: bind=%s gallery=%s db=%s thumbnails=%s log=%s",
		cfg.Server.BindAddress, cfg.Paths.GalleryPath, cfg.Paths.DBPath, cfg.Paths.ThumbnailsPath, cfg.Log.Level)

	if dbDir := filepath.Dir(cfg.Paths.DBPath); dbDir != "" && dbDir != "." {
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			log.Fatalf("FATAL creating db directory %q: %v", dbDir, err)
		}
	}

	database, err := db.Open(cfg.Paths.DBPath)
	if err != nil {
		log.Fatalf("FATAL opening database: %v", err)
	}
	defer database.Close()

	if err := db.Bootstrap(database); err != nil {
		log.Fatalf("FATAL bootstrapping database: %v", err)
	}

	if err := os.MkdirAll(cfg.Paths.ThumbnailsPath, 0o755); err != nil {
		log.Fatalf("FATAL creating thumbnails dir: %v", err)
	}

	// An unreadable gallery path puts the server in degraded mode: sync and watch are disabled.
	degraded := false
	if _, err := os.ReadDir(cfg.Paths.GalleryPath); err != nil {
		logx.Warnf("gallery path %q unreadable: %v — running in degraded mode", cfg.Paths.GalleryPath, err)
		degraded = true
	}

	jobManager := jobs.NewManager()

	srv, err := internalweb.NewServer(cfg, *configPath, database, jobManager, degraded)
	if err != nil {
		log.Fatalf("FATAL creating web server: %v", err)
	}

	if cfg.Gallery.WatchEnabled && !degraded {
		watcher, err := gallery.NewWatcher(cfg, database, jobManager)
		if err != nil {
			if strings.Contains(err.Error(), "too many open files") || strings.Contains(err.Error(), "no space left") {
				logx.Warnf("could not start watcher (inotify limit reached). " +
					"Run on the HOST system (not inside Docker): " +
					"echo fs.inotify.max_user_instances=256 | sudo tee -a /etc/sysctl.conf && sudo sysctl -p. " +
					"Gallery watcher is disabled; use manual sync instead.")
			} else {
				logx.Warnf("could not start watcher: %v", err)
			}
		} else {
			watcher.OnEvent = func(msg string) {
				jobManager.SetWatcherMessage(msg)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				if err := watcher.Run(ctx); err != nil {
					logx.Warnf("watcher stopped: %v", err)
				}
			}()
			logx.Infof("gallery watcher started")
		}
	}

	httpSrv := &http.Server{
		Addr:        cfg.Server.BindAddress,
		Handler:     srv.Handler(),
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is intentionally unset: bulk operations like "delete all search
		// results" or re-extracting metadata across tens of thousands of images can
		// run far longer than any fixed budget. Slow handlers are bounded by DB and
		// filesystem latency, not the HTTP server.
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logx.Infof("monbooru listening on %s", cfg.Server.BindAddress)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("FATAL HTTP server: %v", err)
		}
	}()

	<-quit
	logx.Infof("shutting down…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx) //nolint:errcheck
}
