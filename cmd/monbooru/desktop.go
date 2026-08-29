package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
	internalweb "github.com/monbooru/monbooru/internal/web"
)

// appName is the identity the desktop profile builds its directory names,
// its log filename and its single-instance answer from.
const appName = "monbooru"

// defaultDesktop is stamped at link time by the desktop artifacts, so
// double-clicking one lands in the profile rather than on the container
// defaults. -desktop=false still turns it off.
var defaultDesktop string

// probeTimeout bounds the single-instance question. It is a loopback
// request to a process that either answers immediately or is not there.
const probeTimeout = 500 * time.Millisecond

// desktopSeed adjusts the defaults a config file that does not exist yet is
// written with: OS-native paths in place of the container mounts.
func desktopSeed(l desktop.Layout) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Paths.DataPath = l.DataDir
		cfg.Paths.ModelPath = filepath.Join(l.DataDir, "models")
		// A desktop sleeps through 01:00 more often than not, so a fresh
		// config gets the mode that notices and runs the missed pass.
		cfg.Schedule.Mode = config.ScheduleAtTimeCatchup
		// The log file is the only thing a desktop bug report can carry, and
		// nothing writes at the warn default while the app is healthy.
		cfg.Log.Level = "info"
		cfg.Galleries[0].GalleryPath = seedGalleryDir(l)
	}
}

// seedGalleryDir picks and creates the folder a fresh install watches.
// It is created rather than only named: validate() requires a gallery and
// an unreadable path boots straight into degraded mode, so a seeded-but-
// absent folder would greet a first-time user with a banner.
func seedGalleryDir(l desktop.Layout) string {
	base := desktop.PicturesDir()
	if base == "" {
		base = l.DataDir
	}
	dir := filepath.Join(base, appName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = filepath.Join(l.DataDir, "gallery")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logx.Warnf("could not create a gallery folder at %s: %v", dir, err)
		}
	}
	return dir
}

// claimPort answers a second launch before anything tries to bind. It
// returns proceed=false when this process should stop, and a diagnostic
// when the port belongs to something that is not monbooru. The caller
// decides what a failure costs: at startup nothing is open yet, while the
// shutdown path has a database and watchers to release first.
//
// A 2xx carrying our own name means the operator clicked the menu entry
// twice: show them the instance they already have. Anything else on the
// port is reported by name, because "address already in use" does not tell
// them what to do. No answer at all is the only case where binding is safe.
func claimPort(addr string, openBrowser bool) (string, bool) {
	local := desktop.LoopbackAddr(addr)
	inst, found := desktop.Probe(local, probeTimeout)
	if !found {
		return "", true
	}
	if inst.App == appName {
		// stdout, not the log: a -no-browser launch from a terminal is the
		// one shape of this that has nothing else to show the operator.
		fmt.Printf("%s is already running on http://%s\n", appName, local)
		if openBrowser {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
		}
		return "", false
	}
	other := inst.App
	if other == "" {
		other = "another program"
	}
	return fmt.Sprintf("%s is already serving %s; free the port or set server.bind_address to another one", other, local), false
}

// relaunch starts a fresh process carrying this one's arguments, for the
// Restart control. It runs after the shutdown has released the port and the
// database, so the new process probes an empty port and binds it.
// -no-browser is added because the page that asked for the restart is
// already open and reloads itself; a second tab would be noise.
func relaunch() {
	exe, err := os.Executable()
	if err != nil {
		logx.Errorf("restart: locating this executable: %v", err)
		return
	}
	args := os.Args[1:]
	if !slices.Contains(args, "-no-browser") {
		args = append(slices.Clone(args), "-no-browser")
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		logx.Errorf("restart: %v", err)
		return
	}
	// Nothing here waits on it: this process is on its way out.
	_ = cmd.Process.Release()
}

// dialogOnFatal is set by the desktop profile: a shortcut launch on Windows
// has no console, so a startup failure has to reach a message box or it
// reaches nobody.
var dialogOnFatal bool

// reportFatal puts a startup failure through the log sink - which the
// desktop profile has already pointed at a file - and, on a shortcut
// launch with no console, into a message box.
func reportFatal(msg string) {
	logx.Errorf("FATAL %s", msg)
	if dialogOnFatal {
		desktop.ShowError(appName, msg)
	}
}

// fatalf reports a startup failure and exits non-zero. Only for failures
// early enough that no defer is owed anything.
func fatalf(format string, a ...any) {
	reportFatal(fmt.Sprintf(format, a...))
	os.Exit(1)
}

// explicitFlag returns value only when the operator actually passed the
// flag, so the desktop profile can tell "-config was given" from the flag
// set's own default without changing what -help prints.
func explicitFlag(name, value string) string {
	passed := ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = value
		}
	})
	return passed
}

// versionLine is what -version prints: the version, plus the build stamp
// in parentheses when the artifact carries one.
func versionLine() string {
	line := appName + " " + internalweb.Version
	if label := internalweb.BuildLabel(); label != "" {
		line += " (" + label + ")"
	}
	return line
}

// openWhenServing waits for the listener to answer before opening a
// browser, so a first run that pays a cold start does not land the user on
// a connection-refused page. Gives up quietly: a browser that never opened
// is a smaller problem than one that opened on an error.
func openWhenServing(addr string) {
	local := desktop.LoopbackAddr(addr)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if inst, found := desktop.Probe(local, probeTimeout); found && inst.App == appName {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logx.Warnf("the server did not answer in time; open http://%s yourself", local)
}

// runTray serves the optional tray. It is never the only route to anything
// - Open, start-at-login and Quit are all in Settings - so a platform or a
// build without one is a log line, not a failure.
func runTray(ctx context.Context, srv *internalweb.Server, bindAddr string) {
	local := desktop.LoopbackAddr(bindAddr)
	hook := srv.DesktopHook()
	err := desktop.RunTray(ctx, desktop.TrayMenu{
		Title:    hook.Name,
		IconPath: trayIconPath(),
		Open: func() {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
		},
		Quit:      srv.RequestQuit,
		Autostart: hook,
	})
	if err != nil {
		logx.Infof("tray: %v", err)
	}
}

// trayIconPath is the .ico the Windows artifacts ship beside the binary.
// Elsewhere the icon comes from the desktop theme by name.
func trayIconPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir := fsx.ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, appName+".ico")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
