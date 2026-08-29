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
	_ "time/tzdata"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	internalweb "github.com/monbooru/monbooru/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Subcommand dispatch happens before flag.Parse so the
	// subcommand's own flag set gets the argv tail unchanged.
	if len(os.Args) >= 2 && os.Args[1] == "tagger-worker" {
		runWorker(os.Args[2:])
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "healthcheck" {
		runHealthcheck(os.Args[2:])
		return
	}

	configPath := flag.String("config", "./monbooru.toml", "path to monbooru.toml config file")
	hashPassword := flag.String("hash-password", "", "print bcrypt hash of the given password and exit")
	desktopMode := flag.Bool("desktop", defaultDesktop == "true", "desktop profile: OS-native paths, a log file, a browser at startup")
	noBrowser := flag.Bool("no-browser", false, "with -desktop, do not open a browser at startup")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionLine())
		return
	}
	if *hashPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*hashPassword), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error hashing password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	// Registered before every other defer, so a startup failure still exits
	// non-zero once the rest of the chain has flushed the database pools,
	// stopped the watchers and closed the log.
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	// Registered before every other defer below, so a Restart's fresh
	// process starts only once this one has released the port, the database
	// and the log file.
	restartOnExit := false
	defer func() {
		if restartOnExit {
			relaunch()
		}
	}()

	var seed func(*config.Config)
	var profile internalweb.Desktop
	if *desktopMode {
		dialogOnFatal = true
		layout, err := desktop.Resolve(appName, explicitFlag("config", *configPath))
		if err != nil {
			fatalf("resolving the desktop paths: %v", err)
		}
		*configPath = layout.ConfigPath
		profile = internalweb.Desktop{Active: true, LogDir: layout.LogDir}
		// Before config load, so every fatal below reaches the file too.
		if f, err := desktop.OpenLog(layout.LogDir, appName); err != nil {
			logx.Warnf("could not open a log file under %s: %v", layout.LogDir, err)
		} else {
			defer func() { _ = f.Close() }()
		}
		seed = desktopSeed(layout)
	}

	_, statErr := os.Stat(*configPath)
	freshConfig := os.IsNotExist(statErr)

	cfg, err := config.LoadWithDefaults(*configPath, seed)
	if err != nil {
		fatalf("loading config: %v", err)
	}
	logx.Set(cfg.Log.Level)
	logx.Infof("config: bind=%s galleries=%d default=%q models=%s log=%s",
		cfg.Server.BindAddress, len(cfg.Galleries), cfg.DefaultGallery, cfg.Paths.ModelPath, cfg.Log.Level)

	if *desktopMode {
		if msg, proceed := claimPort(cfg.Server.BindAddress, !*noBrowser); !proceed {
			if msg != "" {
				fatalf("%s", msg)
			}
			return
		}
	}

	jobManager := jobs.NewManager()

	srv, err := internalweb.NewServer(cfg, *configPath, jobManager, profile)
	if err != nil {
		if freshConfig && !*desktopMode {
			log.Printf("monbooru wrote %s with default settings meant for the docker image.", *configPath)
			log.Printf("edit gallery_path (your images), paths.data_path (db + thumbnails) and paths.model_path (optional auto-taggers), then run it again. docs: %s", internalweb.DocURL)
		}
		fatalf("creating web server: %v", err)
	}
	defer srv.Close()

	srv.StartWatchers()

	httpSrv := &http.Server{
		Addr:        cfg.Server.BindAddress,
		Handler:     srv.Handler(),
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is intentionally unset: bulk operations like delete-all
		// or re-extract can run for many minutes on large libraries. Slow
		// handlers are bounded by DB and filesystem latency.
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	srvErr := make(chan error, 1)
	go func() {
		logx.Infof("monbooru listening on %s", cfg.Server.BindAddress)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	if *desktopMode {
		if !*noBrowser {
			go openWhenServing(cfg.Server.BindAddress)
		}
		if srv.TrayEnabled() {
			trayCtx, stopTray := context.WithCancel(context.Background())
			defer stopTray()
			go runTray(trayCtx, srv, cfg.Server.BindAddress)
		}
	}

	// Report a bind failure through the channel rather than log.Fatalf so the
	// deferred srv.Close() still flushes the DB pools and stops the watchers.
	select {
	case <-quit:
		logx.Infof("shutting down...")
	case <-srv.QuitRequested():
		logx.Infof("shutting down on request...")
	case err := <-srvErr:
		logx.Errorf("FATAL HTTP server: %v", err)
		exitCode = 1
		if *desktopMode {
			// Closes the race between two launches that both probed an empty
			// port: whoever lost the bind asks again and gets the winner.
			if msg, _ := claimPort(cfg.Server.BindAddress, !*noBrowser); msg != "" {
				reportFatal(msg)
			}
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx) //nolint:errcheck
	restartOnExit = srv.RestartRequested()
}
