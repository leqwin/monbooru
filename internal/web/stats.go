package web

import (
	"runtime"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/tagger"
)

// rssBreakdown is what the Linux helper returns. anon comes from
// /proc/self/status RssAnon (Pss equals Rss for unshared anon pages);
// total / file / db come from a single /proc/self/smaps walk summing
// Pss so the SQLite per-connection mmap windows over the same DB file
// contribute the unique pages once. The non-Linux stub returns the
// zero value with ok=false so the template hides the corresponding rows.
type rssBreakdown struct {
	total uint64
	anon  uint64
	file  uint64
	db    uint64
}

// memStats is the process-memory snapshot rendered in the Settings → Stats
// section. Sys/HeapAlloc come from runtime.ReadMemStats; Total is Pss
// summed across /proc/self/smaps when the platform exposes it (Linux),
// otherwise zero with Available=false so the template hides the row.
type memStats struct {
	HeapAlloc  int64
	Sys        int64
	Goroutines int
	Total      int64
	// Anon is the process-private slice (Go heap + runtime + stacks +
	// glibc arenas). The kernel can only reclaim this through swap,
	// so it's the part that puts real pressure on host RAM.
	Anon int64
	// GoInuse is the runtime's own share of Anon: the spans and structures
	// actually in use. Not Sys, which measures address space the runtime
	// mapped - arenas and GC metadata it never faulted in included - and so
	// routinely reads larger than the resident Anon it would appear to sit
	// inside.
	GoInuse int64
	// GoIdle is the rest of what the runtime holds from the kernel: spans
	// it has freed but not handed back yet, plus the slack in its stack
	// and span allocators. It drains on its own as the scavenger runs, and
	// is most of what a finished job leaves behind.
	GoIdle int64
	// Native is the slice of Anon neither of the two above accounts for:
	// modernc/libc holding SQLite's page cache, and any other CGO
	// allocation. The auto-tagger's ORT heap lives in the worker
	// subprocess and reports separately under the Auto-tagger row.
	Native int64
	// File is the file-backed slice (mmap'd SQLite DB pages, the
	// binary's text/rodata, shared libs). Cheap under pressure: the
	// kernel evicts it without swap, since the pages are just a cache
	// of files already on disk.
	File int64
	// DB is the slice of File attributed to *.db / -wal / -shm
	// mappings - the SQLite mmap window pages. The remainder of File
	// is the binary text + rodata + shared libs.
	DB        int64
	OtherFile int64
	Available bool
}

// galleryDBStats is one row of the per-gallery DB-size table. DBSize sums
// the SQLite triplet (.db + .db-wal + .db-shm) - WAL can dominate after a
// long write session and is part of "what's on disk for this gallery".
type galleryDBStats struct {
	Name   string
	DBPath string
	DBSize int64
}

// mountStats is one row of the filesystem free-space block. Each unique
// underlying filesystem (deduped by Statfs.fsid in mountUsage) is rendered
// once, with the labels listing every gallery path that resolves to it so
// the operator can map a row back to the galleries it serves.
type mountStats struct {
	Labels    []string
	TotalSize int64
	FreeSize  int64
	UsedSize  int64
	UsedPct   int
}

// taggerCacheStats is the auto-tagger panel rendered under Stats. When
// the cache is cold every field except Loaded is zero, and the template
// shows a single "not loaded" row. Worker* fields surface the
// tagger-worker subprocess's residency so the operator can see where
// the 1 GB+ of inference state is parked.
type taggerCacheStats struct {
	Loaded           bool
	Provider         string
	InUse            bool
	Sessions         []string
	IdleFor          time.Duration // zero when in use
	IdleReleaseAfter time.Duration // 0 == caching disabled
	WorkerPID        int
	WorkerPSS        int64 // bytes; 0 when no worker is alive
	WorkerAnon       int64
	WorkerFile       int64
}

// statsData is the bundle threaded into the settings template.
type statsData struct {
	Mem        memStats
	Galleries  []galleryDBStats
	Mounts     []mountStats
	FSWarnings []string // non-empty when mountUsage failed; rendered as a hint
	Tagger     taggerCacheStats
}

// gatherStats builds the snapshot rendered in the Stats section. Cheap by
// design: a runtime.ReadMemStats, three os.Stat per gallery, one Statfs per
// unique filesystem. No directory walks.
func (s *Server) gatherStats() statsData {
	out := statsData{Mem: gatherMemStats(), Tagger: gatherTaggerStats(s)}

	// Galleries: stable name order for table render. The settings page
	// already shows galleries name-sorted; matching that avoids a "two
	// orderings on one page" mismatch.
	galleries := s.galleryList()
	out.Galleries = make([]galleryDBStats, 0, len(galleries))
	for _, g := range galleries {
		out.Galleries = append(out.Galleries, galleryDBStats{
			Name:   g.Name,
			DBPath: g.DBPath,
			DBSize: dbFileSize(g.DBPath),
		})
	}

	// Mounts: probe each gallery's DB, images, and thumbnails dirs and
	// dedup by the Statfs fsid the platform helper returns. Within one
	// row a gallery is listed once even if its three dirs all resolve
	// there; across rows the same gallery may appear twice if its data
	// is split across filesystems, which is itself useful information.
	type mountAcc struct {
		galleries []string
		seenInRow map[string]bool
		stats     mountStats
	}
	byKey := map[string]*mountAcc{}
	var order []*mountAcc
	addProbe := func(galleryName, path string) {
		if path == "" {
			return
		}
		st, fsid, ok := mountUsage(path)
		if !ok {
			return
		}
		// Some filesystems (overlay, tmpfs on certain kernels) report
		// fsid zero. Key those by path so probes that obviously belong
		// to the same dir tree still merge, while distinct zero-fsid
		// volumes don't get collapsed.
		key := strconv.FormatUint(fsid, 10)
		if fsid == 0 {
			key = "path:" + path
		}
		if acc, ok := byKey[key]; ok {
			if !acc.seenInRow[galleryName] {
				acc.seenInRow[galleryName] = true
				acc.galleries = append(acc.galleries, galleryName)
			}
			return
		}
		acc := &mountAcc{
			galleries: []string{galleryName},
			seenInRow: map[string]bool{galleryName: true},
			stats:     st,
		}
		byKey[key] = acc
		order = append(order, acc)
	}
	for _, g := range galleries {
		addProbe(g.Name, g.DBPath)
		addProbe(g.Name, g.GalleryPath)
		addProbe(g.Name, g.ThumbnailsPath)
	}
	// Second-pass dedup keyed on the size tuple. Statfs sometimes
	// reports distinct fsids for the same physical filesystem (Docker
	// bind mounts and overlay layers do this on Linux), so the fsid
	// dedup above leaves the row repeated. Two filesystems reporting
	// byte-identical TotalSize/FreeSize/UsedSize triples are
	// effectively the same volume from the operator's perspective.
	type sizeKey struct{ Total, Free, Used int64 }
	bySize := map[sizeKey]int{}
	out.Mounts = make([]mountStats, 0, len(order))
	for _, m := range order {
		st := m.stats
		st.Labels = m.galleries
		key := sizeKey{st.TotalSize, st.FreeSize, st.UsedSize}
		if idx, ok := bySize[key]; ok {
			seen := map[string]bool{}
			for _, name := range out.Mounts[idx].Labels {
				seen[name] = true
			}
			for _, name := range st.Labels {
				if !seen[name] {
					seen[name] = true
					out.Mounts[idx].Labels = append(out.Mounts[idx].Labels, name)
				}
			}
			continue
		}
		bySize[key] = len(out.Mounts)
		out.Mounts = append(out.Mounts, st)
	}
	if len(out.Mounts) == 0 {
		out.FSWarnings = append(out.FSWarnings,
			"Filesystem usage unavailable on this platform.")
	}
	return out
}

// gatherTaggerStats snapshots the cache state via tagger.Status() and
// pairs it with the config's idle-release window so the template can
// say when the next idle teardown will fire. The non-tagger build
// returns Loaded=false here, which the template handles by hiding
// every dependent row.
func gatherTaggerStats(s *Server) taggerCacheStats {
	st := tagger.Status()
	out := taggerCacheStats{
		Loaded:   st.Loaded,
		Provider: st.Provider,
		InUse:    st.InUse,
		Sessions: st.Sessions,
	}
	s.cfgMu.Lock()
	mins := s.cfg.Tagger.IdleReleaseAfterMinutes
	s.cfgMu.Unlock()
	if mins > 0 {
		out.IdleReleaseAfter = time.Duration(mins) * time.Minute
	}
	if st.Loaded && !st.InUse && !st.LastUsed.IsZero() {
		out.IdleFor = time.Since(st.LastUsed)
	}
	if pid, ok := tagger.WorkerPID(); ok {
		out.WorkerPID = pid
		if r, rok := procRSSAt(pid); rok {
			out.WorkerPSS = int64(r.total)
			out.WorkerAnon = int64(r.anon)
			out.WorkerFile = int64(r.file)
		}
	}
	return out
}

// gatherMemStats reads runtime.ReadMemStats and asks procRSS for the
// per-process residency breakdown. procRSS returns ok=false on platforms
// without a cheap RSS source; the template hides the dependent rows.
func gatherMemStats() memStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// What the runtime still holds from the kernel. GCSys, OtherSys and
	// BuckHashSys are left out: they are mapped far larger than they are
	// ever faulted in, and counting them puts the runtime's share above
	// RssAnon and the native remainder to zero.
	goResident := int64(ms.HeapSys-ms.HeapReleased) + int64(ms.StackSys+ms.MSpanSys+ms.MCacheSys)
	out := memStats{
		HeapAlloc:  int64(ms.HeapAlloc),
		Sys:        int64(ms.Sys),
		GoInuse:    int64(ms.HeapInuse + ms.StackInuse + ms.MSpanInuse + ms.MCacheInuse),
		Goroutines: runtime.NumGoroutine(),
	}
	out.GoIdle = goResident - out.GoInuse
	if r, ok := procRSS(); ok {
		out.Total = int64(r.total)
		out.Anon = int64(r.anon)
		out.File = int64(r.file)
		out.DB = int64(r.db)
		out.OtherFile = max(out.File-out.DB, 0)
		// Clamped: the runtime's accounting and the kernel's RssAnon are
		// taken a moment apart, and a page swapped out leaves one and not
		// the other.
		out.Native = max(out.Anon-goResident, 0)
		out.Available = true
	}
	return out
}
