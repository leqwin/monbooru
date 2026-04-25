package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds all application configuration.
type Config struct {
	DefaultGallery string         `toml:"default_gallery"`
	Galleries      []Gallery      `toml:"galleries"`
	Server         ServerConfig   `toml:"server"`
	Paths          PathsConfig    `toml:"paths"`
	Gallery        GalleryConfig  `toml:"gallery"`
	Tagger         TaggerConfig   `toml:"tagger"`
	Auth           AuthConfig     `toml:"auth"`
	UI             UIConfig       `toml:"ui"`
	Log            LogConfig      `toml:"log"`
	Schedule       ScheduleConfig `toml:"schedule"`
}

type ServerConfig struct {
	BindAddress string `toml:"bind_address"`
	BaseURL     string `toml:"base_url"`
}

// PathsConfig holds process-wide paths. Per-gallery DB and thumbnails
// paths are derived from DataPath + the gallery name.
type PathsConfig struct {
	DataPath  string `toml:"data_path"`
	ModelPath string `toml:"model_path"`
}

// Gallery is one named gallery. Only Name and GalleryPath persist;
// DBPath and ThumbnailsPath are derived at Load time.
type Gallery struct {
	Name           string `toml:"name"`
	GalleryPath    string `toml:"gallery_path"`
	DBPath         string `toml:"-"`
	ThumbnailsPath string `toml:"-"`
}

type GalleryConfig struct {
	WatchEnabled  bool `toml:"watch_enabled"`
	MaxFileSizeMB int  `toml:"max_file_size_mb"`
}

type TaggerConfig struct {
	UseCUDA  bool             `toml:"use_cuda"`
	Parallel int              `toml:"parallel"`
	Taggers  []TaggerInstance `toml:"taggers"`
}

type TaggerInstance struct {
	Name                string  `toml:"name"`
	Enabled             bool    `toml:"enabled"`
	ConfidenceThreshold float64 `toml:"confidence_threshold"`
	ModelFile           string  `toml:"model_file"`
	TagsFile            string  `toml:"tags_file"`
}

type AuthConfig struct {
	EnablePassword      bool   `toml:"enable_password"`
	PasswordHash        string `toml:"password_hash"`
	SessionLifetimeDays int    `toml:"session_lifetime_days"`
	APIToken            string `toml:"api_token"`
}

type UIConfig struct {
	PageSize int `toml:"page_size"`
}

// LogConfig controls log verbosity: "warn" (default), "info", "debug".
type LogConfig struct {
	Level string `toml:"level"`
}

// ScheduleConfig drives the once-per-day maintenance run at HH:MM on
// every configured gallery.
type ScheduleConfig struct {
	Time             string `toml:"time"` // "HH:MM" 24h, default "01:00"
	SyncGallery      bool   `toml:"sync_gallery"`
	RemoveOrphans    bool   `toml:"remove_orphans"`
	RunAutoTaggers   bool   `toml:"run_auto_taggers"`
	RecomputeTags    bool   `toml:"recompute_tags"`
	MergeGeneralTags bool   `toml:"merge_general_tags"`
	VacuumDB         bool   `toml:"vacuum_db"`
}

// Default returns a fully populated config with all spec defaults.
func Default() *Config {
	return &Config{
		DefaultGallery: "default",
		Galleries: []Gallery{{
			Name:        "default",
			GalleryPath: "/gallery",
		}},
		Server: ServerConfig{
			BindAddress: "127.0.0.1:8080",
			BaseURL:     "http://localhost:8080",
		},
		Paths: PathsConfig{
			DataPath:  "/data",
			ModelPath: "/models",
		},
		Gallery: GalleryConfig{
			WatchEnabled:  true,
			MaxFileSizeMB: 500,
		},
		Tagger: TaggerConfig{Parallel: 16},
		Auth: AuthConfig{
			SessionLifetimeDays: 7,
		},
		UI: UIConfig{
			PageSize: 40,
		},
		Log: LogConfig{
			Level: "warn",
		},
		Schedule: ScheduleConfig{
			Time:             "01:00",
			SyncGallery:      true,
			RemoveOrphans:    true,
			RunAutoTaggers:   true,
			RecomputeTags:    true,
			MergeGeneralTags: true,
			VacuumDB:         true,
		},
	}
}

var scheduleTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// ValidateScheduleTime accepts "HH:MM" in 24-hour form.
func ValidateScheduleTime(v string) error {
	if !scheduleTimeRe.MatchString(v) {
		return fmt.Errorf("schedule.time %q must be HH:MM (00:00–23:59)", v)
	}
	return nil
}

// Load reads and decodes a TOML config file. If absent, creates it with defaults.
func Load(path string) (*Config, error) {
	cfg := Default()

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		if writeErr := Save(cfg, path); writeErr != nil {
			return nil, fmt.Errorf("creating default config: %w", writeErr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("checking config file: %w", err)
	} else {
		cfg.Galleries = nil
		cfg.DefaultGallery = ""
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}
	fillDerivedPaths(cfg)
	applyEnvOverrides(cfg)
	return cfg, nil
}

// Save marshals cfg to TOML and writes atomically to path.
func Save(cfg *Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, ".monbooru.toml.*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	if err := toml.NewEncoder(tmpFile).Encode(cfg); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// FindGallery returns the gallery with the given name, or nil.
func (cfg *Config) FindGallery(name string) *Gallery {
	for i := range cfg.Galleries {
		if cfg.Galleries[i].Name == name {
			return &cfg.Galleries[i]
		}
	}
	return nil
}

// DerivePaths returns the canonical DB and thumbnails paths for a gallery.
// Each gallery lives under <data_path>/<name>/.
func (cfg *Config) DerivePaths(name string) (dbPath, thumbnailsPath string) {
	dir := filepath.Join(cfg.Paths.DataPath, name)
	return filepath.Join(dir, "monbooru.db"), filepath.Join(dir, "thumbnails")
}

var galleryNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateGalleryName rejects empty names or characters unsafe in a filename.
func ValidateGalleryName(name string) error {
	if name == "" {
		return fmt.Errorf("gallery name must not be empty")
	}
	if !galleryNameRe.MatchString(name) {
		return fmt.Errorf("gallery name %q must match [A-Za-z0-9_-]+", name)
	}
	return nil
}

// fillDerivedPaths populates DBPath and ThumbnailsPath for every gallery.
func fillDerivedPaths(cfg *Config) {
	for i := range cfg.Galleries {
		db, th := cfg.DerivePaths(cfg.Galleries[i].Name)
		cfg.Galleries[i].DBPath = db
		cfg.Galleries[i].ThumbnailsPath = th
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("MONBOORU_SERVER_BIND_ADDRESS"); v != "" {
		cfg.Server.BindAddress = v
	}
	if v := os.Getenv("MONBOORU_SERVER_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	}
	if v := os.Getenv("MONBOORU_PATHS_DATA_PATH"); v != "" {
		cfg.Paths.DataPath = v
		fillDerivedPaths(cfg)
	}
	if v := os.Getenv("MONBOORU_PATHS_MODEL_PATH"); v != "" {
		cfg.Paths.ModelPath = v
	}
	if v := os.Getenv("MONBOORU_GALLERY_WATCH_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Gallery.WatchEnabled = b
		}
	}
	if v := os.Getenv("MONBOORU_GALLERY_MAX_FILE_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Gallery.MaxFileSizeMB = n
		}
	}
	if v := os.Getenv("MONBOORU_TAGGER_USE_CUDA"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Tagger.UseCUDA = b
		}
	}
	if v := os.Getenv("MONBOORU_AUTH_ENABLE_PASSWORD"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Auth.EnablePassword = b
		}
	}
	if v := os.Getenv("MONBOORU_AUTH_PASSWORD_HASH"); v != "" {
		cfg.Auth.PasswordHash = v
	}
	if v := os.Getenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Auth.SessionLifetimeDays = n
		}
	}
	if v := os.Getenv("MONBOORU_AUTH_API_TOKEN"); v != "" {
		cfg.Auth.APIToken = v
	}
	if v := os.Getenv("MONBOORU_UI_PAGE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.UI.PageSize = n
		}
	}
	if v := os.Getenv("MONBOORU_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
}

func validate(cfg *Config) error {
	if cfg.Server.BindAddress == "" {
		return fmt.Errorf("server.bind_address must not be empty")
	}
	if !strings.Contains(cfg.Server.BindAddress, ":") {
		return fmt.Errorf("server.bind_address %q is not a valid host:port", cfg.Server.BindAddress)
	}
	// enable_password=true with an empty hash would let the password-update
	// handler bypass the current-password check (that guard only runs when
	// PasswordHash != "").
	if cfg.Auth.EnablePassword && strings.TrimSpace(cfg.Auth.PasswordHash) == "" {
		return fmt.Errorf("auth.enable_password is true but auth.password_hash is empty - " +
			"run `monbooru -hash-password 'your-password'` and paste the result into monbooru.toml")
	}
	if len(cfg.Galleries) == 0 {
		return fmt.Errorf("at least one gallery must be configured")
	}
	if cfg.Paths.DataPath == "" {
		return fmt.Errorf("paths.data_path must not be empty")
	}
	seen := map[string]bool{}
	for _, g := range cfg.Galleries {
		if err := ValidateGalleryName(g.Name); err != nil {
			return fmt.Errorf("invalid gallery: %w", err)
		}
		if seen[g.Name] {
			return fmt.Errorf("duplicate gallery name %q", g.Name)
		}
		seen[g.Name] = true
		if g.GalleryPath == "" {
			return fmt.Errorf("gallery %q has an empty gallery_path", g.Name)
		}
	}
	if cfg.DefaultGallery == "" {
		cfg.DefaultGallery = cfg.Galleries[0].Name
	} else if cfg.FindGallery(cfg.DefaultGallery) == nil {
		cfg.DefaultGallery = cfg.Galleries[0].Name
	}
	if cfg.Schedule.Time == "" {
		cfg.Schedule.Time = "01:00"
	} else if err := ValidateScheduleTime(cfg.Schedule.Time); err != nil {
		return err
	}
	return nil
}
