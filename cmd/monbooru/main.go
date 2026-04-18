package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leqwin/monbooru/internal/config"
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
	logx.Infof("config: bind=%s galleries=%d default=%q models=%s log=%s",
		cfg.Server.BindAddress, len(cfg.Galleries), cfg.DefaultGallery, cfg.Paths.ModelPath, cfg.Log.Level)

	jobManager := jobs.NewManager()

	srv, err := internalweb.NewServer(cfg, *configPath, jobManager)
	if err != nil {
		log.Fatalf("FATAL creating web server: %v", err)
	}
	defer srv.Close()

	// Start the watcher on the active gallery. Subsequent switches start/stop
	// watchers inside the Server so the gallery context owns that lifecycle.
	srv.StartActiveWatcher()

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
