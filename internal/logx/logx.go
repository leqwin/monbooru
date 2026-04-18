// Package logx is a thin level gate over the stdlib log package.
//
// Three levels: "warn" (the default; warn+error only), "info" (one line per
// user request plus startup banners), and "debug" (everything including
// static-asset hits). Warnings, errors and fatals always fire regardless of
// level - only Infof and Debugf respect the gate.
package logx

import (
	"log"
	"strings"
	"sync/atomic"
)

// Level is an ordinal verbosity.
type Level int32

const (
	LevelWarn Level = iota
	LevelInfo
	LevelDebug
)

// level is read-lock-free. Callers set once at startup via Set.
var level atomic.Int32

// Set parses a name ("warn", "info", "debug"; anything else becomes "warn")
// and installs it as the current level.
func Set(name string) {
	var l Level
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "info":
		l = LevelInfo
	case "debug":
		l = LevelDebug
	default:
		l = LevelWarn
	}
	level.Store(int32(l))
}

// Enabled reports whether messages at l would be emitted.
func Enabled(l Level) bool { return l <= Level(level.Load()) }

// Debugf prints only at debug level.
func Debugf(format string, a ...any) {
	if Enabled(LevelDebug) {
		log.Printf("DEBUG "+format, a...)
	}
}

// Infof prints at info or debug level.
func Infof(format string, a ...any) {
	if Enabled(LevelInfo) {
		log.Printf("INFO "+format, a...)
	}
}

// Warnf always prints.
func Warnf(format string, a ...any) { log.Printf("WARN "+format, a...) }

// Errorf always prints.
func Errorf(format string, a ...any) { log.Printf("ERROR "+format, a...) }
