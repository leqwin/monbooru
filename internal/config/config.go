package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds all application configuration.
type Config struct {
	Server  ServerConfig  `toml:"server"`
	Paths   PathsConfig   `toml:"paths"`
	Gallery GalleryConfig `toml:"gallery"`
	Tagger  TaggerConfig  `toml:"tagger"`
	Auth    AuthConfig    `toml:"auth"`
	UI      UIConfig      `toml:"ui"`
	Log     LogConfig     `toml:"log"`
}

type ServerConfig struct {
	BindAddress string `toml:"bind_address"`
	BaseURL     string `toml:"base_url"`
}

type PathsConfig struct {
	GalleryPath    string `toml:"gallery_path"`
	DBPath         string `toml:"db_path"`
	ThumbnailsPath string `toml:"thumbnails_path"`
	ModelPath      string `toml:"model_path"`
}

type GalleryConfig struct {
	WatchEnabled  bool `toml:"watch_enabled"`
	MaxFileSizeMB int  `toml:"max_file_size_mb"`
}

type TaggerConfig struct {
	UseCUDA  bool             `toml:"use_cuda"`
	Parallel int              `toml:"parallel"` // number of concurrent auto-tagging workers
	Taggers  []TaggerInstance `toml:"taggers"`
}

// TaggerInstance describes one auto-tagger living under a subfolder of
// paths.model_path. Multiple instances can be enabled; enabled taggers are
// applied in order at auto-tag time and their outputs are deduplicated.
type TaggerInstance struct {
	Name                string  `toml:"name"`                 // subfolder name under model_path
	Enabled             bool    `toml:"enabled"`
	ConfidenceThreshold float64 `toml:"confidence_threshold"` // per-tagger threshold
	ModelFile           string  `toml:"model_file"`           // optional override; default "model.onnx"
	TagsFile            string  `toml:"tags_file"`            // optional override; default "tags.csv"
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

// LogConfig controls log verbosity. Accepted levels:
//   - "warn":  only warnings, errors and explicit mutations (auth, settings).
//   - "info":  the above plus one line per user request and startup banners.
//   - "debug": info plus noisy traffic (static assets, thumbnails, health).
type LogConfig struct {
	Level string `toml:"level"`
}

// Default returns a fully populated config with all spec defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			BindAddress: "127.0.0.1:8080",
			BaseURL:     "http://localhost:8080",
		},
		Paths: PathsConfig{
			GalleryPath:    "/gallery",
			DBPath:         "/data/monbooru.db",
			ThumbnailsPath: "/data/thumbnails",
			ModelPath:      "/models",
		},
		Gallery: GalleryConfig{
			WatchEnabled:  true,
			MaxFileSizeMB: 500,
		},
		Tagger: TaggerConfig{Parallel: 16},
		Auth: AuthConfig{
			EnablePassword:      false,
			PasswordHash:        "",
			SessionLifetimeDays: 7,
			APIToken:            "",
		},
		UI: UIConfig{
			PageSize: 40,
		},
		Log: LogConfig{
			Level: "warn",
		},
	}
}

// Load reads and decodes a TOML config file. If absent, creates it with defaults.
// Env var overrides are applied after file parsing.
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
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}

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

	enc := toml.NewEncoder(tmpFile)
	if err := enc.Encode(cfg); err != nil {
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

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("MONBOORU_SERVER_BIND_ADDRESS"); v != "" {
		cfg.Server.BindAddress = v
	}
	if v := os.Getenv("MONBOORU_SERVER_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	}
	if v := os.Getenv("MONBOORU_PATHS_GALLERY_PATH"); v != "" {
		cfg.Paths.GalleryPath = v
	}
	if v := os.Getenv("MONBOORU_PATHS_DB_PATH"); v != "" {
		cfg.Paths.DBPath = v
	}
	if v := os.Getenv("MONBOORU_PATHS_THUMBNAILS_PATH"); v != "" {
		cfg.Paths.ThumbnailsPath = v
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
	// Per-tagger settings live in the Settings page, not env vars. The GPU toggle
	// is env-overridable so containerised GPU deployments can flip it declaratively.
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
	// enable_password=true with an empty hash would let the password-update handler
	// bypass the current-password check (that guard only runs when PasswordHash != "").
	// Refuse to start rather than silently open the door.
	if cfg.Auth.EnablePassword && strings.TrimSpace(cfg.Auth.PasswordHash) == "" {
		return fmt.Errorf("auth.enable_password is true but auth.password_hash is empty — " +
			"run `monbooru -hash-password 'your-password'` and paste the result into monbooru.toml")
	}
	return nil
}
