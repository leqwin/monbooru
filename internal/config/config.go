package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// Config holds all application configuration.
type Config struct {
	DefaultGallery string          `toml:"default_gallery"`
	Galleries      []Gallery       `toml:"galleries,omitempty"`
	Server         ServerConfig    `toml:"server"`
	Monloader      MonloaderConfig `toml:"monloader"`
	Paths          PathsConfig     `toml:"paths"`
	Gallery        GalleryConfig   `toml:"gallery"`
	Tagger         TaggerConfig    `toml:"tagger"`
	Auth           AuthConfig      `toml:"auth"`
	UI             UIConfig        `toml:"ui"`
	Log            LogConfig       `toml:"log"`
	Schedule       ScheduleConfig  `toml:"schedule"`
	Relations      RelationsConfig `toml:"relations"`
	// Emptied arrays of tables are omitted rather than written as
	// `key = []`: TOML refuses a later `[[key]]` block under a key already
	// bound to an inline array, so a file monbooru wrote would stop loading
	// the moment a block was put back by hand.
	Plugins []PluginConfig `toml:"plugin,omitempty"`
}

// RelationsConfig drives the relations feature's runtime knobs.
// default_distance is the Hamming-distance default for find-pairs and
// the on-ingest incremental probe; default_session_order picks which mode
// the Start-session CTA preselects; incremental_on_ingest toggles the
// on-ingest BK-tree probe that fans new pairs into the queue.
// DefaultTagPairThreshold is the admission score for tag-similarity
// pairs, clamped to [MinTagPairThreshold, 1]. 0.85 keeps the queue
// proportional across library sizes - a few hundred pairs on a small
// library, a few thousand on a large one - while the 0.7 floor stops a
// setting that would offer more pairs than anyone can decide.
const (
	DefaultTagPairThreshold = 0.85
	MinTagPairThreshold     = 0.7
)

// ClampTagPairThreshold keeps a hand-edited or form-posted threshold
// inside the usable band.
func ClampTagPairThreshold(v float64) float64 {
	switch {
	case v < MinTagPairThreshold:
		return MinTagPairThreshold
	case v > 1:
		return 1
	}
	return v
}

// tag_pairs adds the tag-similarity pass to find-pairs;
// tag_pair_threshold is the score a pair must reach to be queued.
type RelationsConfig struct {
	DefaultDistance     int     `toml:"default_distance"`
	DefaultSessionOrder string  `toml:"default_session_order"`
	IncrementalOnIngest bool    `toml:"incremental_on_ingest"`
	TagPairs            bool    `toml:"tag_pairs"`
	TagPairThreshold    float64 `toml:"tag_pair_threshold"`
}

type ServerConfig struct {
	BindAddress string `toml:"bind_address"`
	BaseURL     string `toml:"base_url"`
	// CustomCSS is an optional absolute path to a stylesheet that the
	// operator drops next to monbooru.toml. When set, it is served at
	// /custom.css and linked from the layout after the bundled main.css
	// so :root overrides win the cascade.
	CustomCSS string `toml:"custom_css"`
	// BooruName overrides the brand shown in every page <title>, the
	// topbar wordmark, and the login screen. Empty resolves to "Monbooru"
	// at render time so existing libraries upgrade without a config edit.
	BooruName string `toml:"name"`
	// BooruLogo is an optional absolute path to a logo / favicon image
	// served at /custom.logo. When set, it replaces both the favicon link
	// and the topbar logo on every page. Path scope is gated at config
	// load against the same trusted-roots check as CustomCSS.
	BooruLogo string `toml:"logo"`
	// Theme names an operator-installed theme under <configdir>/themes/ - a
	// folder holding theme.css (and optionally logo.png) or a bare
	// <name>.css. Always a basename, validated against the folder listing
	// before it reaches ServeFile; empty is the shipped look.
	Theme string `toml:"theme,omitempty"`
	// ThemeColor is the #rgb / #rrggbb the web manifest reports as the
	// splash and address-bar colour. The server can't read a custom
	// stylesheet's palette, so a retheme sets this to its own background.
	// Empty resolves to the bundled palette at render time.
	ThemeColor string `toml:"theme_color"`
	// MonloaderURL is the browser-facing base of the companion monloader
	// instance; the footer "connected to monloader" link points at its queue.
	// Blank falls back to the api url.
	MonloaderURL string `toml:"monloader_url"`
}

// MonloaderConfig is the server-side connection to the companion monloader,
// used for the connectivity light and source refetches. APIURL is an optional
// operator override for the LAN base monbooru calls; left blank, the address is
// auto-detected at pairing from the source the request came from. APIToken is
// the token monloader issued during pairing. The browser-facing link stays in
// server.monloader_url.
type MonloaderConfig struct {
	APIURL   string `toml:"api_url"`
	APIToken string `toml:"api_token,omitempty"`
	// Paused suspends every call to monloader without dropping the pairing,
	// so the footer light's kill switch is reversible: the credentials
	// survive and re-enabling resumes connectivity with no re-pair.
	Paused bool `toml:"paused,omitempty"`
}

// PluginConfig is one third-party peer, written by the pairing claim and by
// the enable switch on a dropped folder's row - never by hand. APIURL
// overrides the address learned at pairing, mirroring [monloader].api_url.
// No launch line lives here: a plugin monbooru runs is a folder under
// <configdir>/plugins/ carrying its own manifest, so a block never names a
// program.
type PluginConfig struct {
	Name      string `toml:"name"`
	Version   string `toml:"version,omitempty"`
	APIURL    string `toml:"api_url,omitempty"`
	PeerToken string `toml:"peer_token,omitempty"`
	Paused    bool   `toml:"paused,omitempty"`
	// Enabled is the boot-start switch for a plugin discovered under the
	// plugins folder; its launch line lives in the folder's manifest, so the
	// block only records the operator's start/stop choice.
	Enabled bool           `toml:"enabled,omitempty"`
	Buttons []PluginButton `toml:"button,omitempty"`
}

// PluginButton is one entry a peer renders in a mount point. Path joins the
// peer's base address. Media is a comma-joined list of models.MediaKinds
// limiting the button to what the peer can actually work on.
type PluginButton struct {
	Slot  string `toml:"slot" json:"slot"`
	Label string `toml:"label" json:"label"`
	Mode  string `toml:"mode" json:"mode"`
	Path  string `toml:"path,omitempty" json:"path,omitempty"`
	Media string `toml:"media,omitempty" json:"media,omitempty"`
}

// AppliesTo reports whether the button covers a file type. A button naming
// no media takes everything.
func (b PluginButton) AppliesTo(fileType string) bool {
	if b.Media == "" {
		return true
	}
	kind := models.MediaKind(fileType)
	for _, v := range strings.Split(b.Media, ",") {
		if strings.TrimSpace(v) == kind {
			return true
		}
	}
	return false
}

// The mount points and button modes plugins may declare.
const (
	SlotDetailActions = "detail-actions"
	SlotBatchBar      = "batch-bar"
	ModeOpen          = "open"
	ModeRelay         = "relay"
)

// MaxPluginLabel caps a button label, and MaxPluginVersion the peer's
// self-reported version; both are rendered as text on operator surfaces.
const (
	MaxPluginLabel      = 24
	MaxPluginVersion    = 32
	maxPluginSlotButton = 4
)

// PluginVars are the substitution variables an open-mode target may carry.
var PluginVars = []string{"{image_id}", "{gallery}", "{back_url}"}

var (
	pluginNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	pluginVarRe  = regexp.MustCompile(`\{[^}]*\}`)
)

// reservedPluginNames name monbooru itself; a peer taking one would make
// every log line and settings row ambiguous.
var reservedPluginNames = []string{"monbooru", "api", "default"}

// ValidatePluginName gates the string that becomes a TOML block key, a
// config-map key, a log prefix and settings-row text.
func ValidatePluginName(name string) error {
	if !pluginNameRe.MatchString(name) {
		return fmt.Errorf("plugin name %q must match [A-Za-z0-9_-]{1,32}", name)
	}
	if slices.Contains(reservedPluginNames, strings.ToLower(name)) {
		return fmt.Errorf("plugin name %q is reserved", name)
	}
	return nil
}

// ValidatePluginButton rejects anything monbooru could not render or route.
func ValidatePluginButton(b PluginButton) error {
	if b.Slot != SlotDetailActions && b.Slot != SlotBatchBar {
		return fmt.Errorf("unknown slot %q", b.Slot)
	}
	if b.Mode != ModeOpen && b.Mode != ModeRelay {
		return fmt.Errorf("unknown mode %q", b.Mode)
	}
	// A batch-bar click carries a scope, which a page cannot receive.
	if b.Mode == ModeOpen && b.Slot != SlotDetailActions {
		return fmt.Errorf("open mode is only valid on %s", SlotDetailActions)
	}
	label := strings.TrimSpace(b.Label)
	if label == "" || len(label) > MaxPluginLabel {
		return fmt.Errorf("label must be 1-%d characters", MaxPluginLabel)
	}
	if b.Media != "" {
		for _, v := range strings.Split(b.Media, ",") {
			if kind := strings.TrimSpace(v); !slices.Contains(models.MediaKinds, kind) {
				return fmt.Errorf("unknown media %q", kind)
			}
		}
	}
	// Relay posts to the peer base when no path is declared; an open link
	// has nowhere to go without one.
	if b.Path == "" && b.Mode == ModeOpen {
		return fmt.Errorf("open mode needs a path")
	}
	if b.Path != "" && !strings.HasPrefix(b.Path, "/") {
		return fmt.Errorf("path %q must start with /", b.Path)
	}
	for _, v := range pluginVarRe.FindAllString(b.Path, -1) {
		if !slices.Contains(PluginVars, v) {
			return fmt.Errorf("unknown substitution variable %s", v)
		}
	}
	return nil
}

// ValidatePluginButtons validates each button and caps how many one peer
// may put in a single slot.
func ValidatePluginButtons(buttons []PluginButton) error {
	perSlot := map[string]int{}
	for _, b := range buttons {
		if err := ValidatePluginButton(b); err != nil {
			return err
		}
		perSlot[b.Slot]++
		if perSlot[b.Slot] > maxPluginSlotButton {
			return fmt.Errorf("at most %d buttons per slot", maxPluginSlotButton)
		}
	}
	return nil
}

// FindPairedToken returns the token issued to the named peer, or nil. The
// pairing flow writes at most one per peer. Callers hold the config lock.
func (cfg *Config) FindPairedToken(app string) *Token {
	for i := range cfg.Auth.Tokens {
		if cfg.Auth.Tokens[i].Paired == app {
			return &cfg.Auth.Tokens[i]
		}
	}
	return nil
}

// FindPlugin returns the block with the given name, or nil.
func (cfg *Config) FindPlugin(name string) *PluginConfig {
	for i := range cfg.Plugins {
		if cfg.Plugins[i].Name == name {
			return &cfg.Plugins[i]
		}
	}
	return nil
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
	// DefaultUploadFolder is the subfolder received files land in when the
	// request leaves the folder blank. Relative to the active gallery
	// root so one global value works across galleries; empty means root.
	// Accepts the filename tokens (gallery.ParseNameTemplate).
	DefaultUploadFolder string `toml:"default_upload_folder"`
	// DefaultUploadName renames a received file once its row exists, which
	// is what makes {id} and {hash} available. Empty keeps the name the
	// sender gave.
	DefaultUploadName string `toml:"default_upload_name"`
	// RenameOnIngest extends DefaultUploadName to what the watcher and a
	// sync pick up off the filesystem. Off by default: a file the operator
	// dropped in themselves keeps the name they gave it.
	RenameOnIngest bool `toml:"rename_on_ingest"`
}

type TaggerConfig struct {
	// UseCUDA is the legacy boolean GPU toggle, replaced by ExecutionProvider.
	// It is kept only for backward-compatible loading of old configs and is
	// never written back to disk.
	UseCUDA bool `toml:"use_cuda,omitempty"`
	// ExecutionProvider selects the ONNX Runtime execution provider.
	// Valid values: cpu, cuda, directml, tensorrt, openvino, coreml, coremlv2.
	// Empty is treated as "cpu". On read, use_cuda=true with no execution_provider
	// is migrated to "cuda".
	ExecutionProvider string `toml:"execution_provider"`
	Parallel          int    `toml:"parallel"`
	// IdleReleaseAfterMinutes is how long the cached ORT session may sit
	// idle before the reclaim loop tears it down. 0 disables caching, so
	// every run loads the model fresh. Default 15.
	IdleReleaseAfterMinutes int                  `toml:"idle_release_after_minutes"`
	Aggregation             TaggerAggregationCfg `toml:"aggregation"`
	Taggers                 []TaggerInstance     `toml:"taggers"`
}

// ValidExecutionProviders lists the ONNX Runtime execution providers the
// operator may select. Kept lowercase to match TOML and wire values.
var ValidExecutionProviders = []string{"cpu", "cuda", "directml", "tensorrt", "openvino", "coreml", "coremlv2"}

// IsValidExecutionProvider reports whether v is a recognized provider name.
func IsValidExecutionProvider(v string) bool {
	return slices.Contains(ValidExecutionProviders, v)
}

// TaggerAggregationCfg holds the frame-merge knob shared across every
// configured tagger. MinHitFraction is the fraction of frames a label
// must score above the pre-floor on to survive the per-row merge.
// Resolves to min_hits = clamp(ceil(MinHitFraction * frame_count), 2, 10),
// degrading to 1 when frame_count == 1 (single image). MinHitFraction
// = 0 sets min_hits = 1 (a single hit is enough); the stored
// confidence is always the mean over the frames the label hit.
type TaggerAggregationCfg struct {
	MinHitFraction float64 `toml:"min_hit_fraction"`
}

type TaggerInstance struct {
	Name                string  `toml:"name"`
	Enabled             bool    `toml:"enabled"`
	ConfidenceThreshold float64 `toml:"confidence_threshold"`
	ModelFile           string  `toml:"model_file"`
	TagsFile            string  `toml:"tags_file"`
	// CategoryThresholds overrides ConfidenceThreshold per destination
	// category. A label whose resolved category name appears as a key
	// uses that threshold; missing keys fall back to the global one.
	// Operator-managed via Settings → Auto-Tagger → Configure.
	CategoryThresholds map[string]float64 `toml:"category_thresholds,omitempty"`
	// PerCategoryTopK caps the number of tags this tagger may emit per
	// category after thresholding. Map key is the category name; value
	// 0 disables the cap for that category on this tagger. Missing
	// keys fall back to the built-in default table (character=8,
	// copyright=4, artist=4, general=25, rating=1, other=10).
	// Operator-managed via Settings → Auto-Tagger → Configure.
	PerCategoryTopK map[string]int `toml:"per_category_top_k,omitempty"`
	// DisabledCategories names categories this tagger must not emit. A
	// label whose resolved category appears here is dropped during
	// aggregation regardless of its score, so the operator can run a
	// tagger for only a subset of its categories. Empty / missing means
	// every emitted category is kept.
	// Operator-managed via Settings → Auto-Tagger → Configure.
	DisabledCategories []string `toml:"disabled_categories,omitempty"`
	// Galleries restricts this tagger to a named subset of galleries.
	// Three persisted shapes:
	//   - missing in TOML (decodes to nil) - applies to every gallery,
	//     including ones added later. The legacy default.
	//   - galleries = []                  - applies to no gallery; the
	//     tagger stays configured but dormant.
	//   - galleries = [...]               - applies only to those names.
	// `omitempty` is intentionally absent so an explicit empty slice
	// survives a write/read round-trip; without it BurntSushi collapses
	// nil and empty into the same wire shape.
	// Operator-managed via Settings → Auto-Tagger → Galleries.
	Galleries []string `toml:"galleries"`
}

// AppliesToGallery reports whether this tagger should run on the named
// gallery. Nil Galleries means "every gallery" (matches the pre-feature
// behaviour); a non-nil slice gates by exact name match - including the
// explicit-empty case, which means "no gallery".
func (t TaggerInstance) AppliesToGallery(name string) bool {
	if t.Galleries == nil {
		return true
	}
	return slices.Contains(t.Galleries, name)
}

type AuthConfig struct {
	EnablePassword      bool    `toml:"enable_password"`
	PasswordHash        string  `toml:"password_hash"`
	SessionLifetimeDays int     `toml:"session_lifetime_days"`
	Tokens              []Token `toml:"tokens,omitempty"`
}

// API privilege scopes. A token grants any combination; new tokens default
// to all of them.
const (
	ScopeRead   = "read"
	ScopeWrite  = "write"
	ScopeDelete = "delete"
)

// AllScopes is every scope a monbooru token can hold.
var AllScopes = []string{ScopeRead, ScopeWrite, ScopeDelete}

// Token is a named API credential. Only the secret's hash is stored; the
// plaintext is shown once at creation. Paired is set by the monloader pairing
// flow and names the peer; it is empty for operator-created tokens.
type Token struct {
	ID        string   `toml:"id"`
	Name      string   `toml:"name"`
	TokenHash string   `toml:"token_hash"`
	Scopes    []string `toml:"scopes"`
	CreatedAt string   `toml:"created_at"`
	Paired    string   `toml:"paired,omitempty"`
	PeerURL   string   `toml:"peer_url,omitempty"`
}

// HasScope reports whether the token carries the given scope.
func (t Token) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

// HashToken returns the hex SHA-256 of a bearer secret.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// GenerateSecret returns a fresh 32-character hex bearer secret.
func GenerateSecret() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func newTokenID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// reservedTokenName matches names the pairing flow owns, so an operator
// cannot create one that collides with or impersonates a paired token.
func reservedTokenName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "(paired)")
}

// ValidateTokenName rejects empty and pairing-reserved names.
func ValidateTokenName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("token name must not be empty")
	}
	if reservedTokenName(n) {
		return fmt.Errorf("token names ending in \"(paired)\" are reserved")
	}
	return nil
}

// GenerateToken builds a token from a name and scopes, returning the plaintext
// secret (available only here). Call it outside any locked or replayed config
// mutation so the id, secret, and timestamp are stable.
func GenerateToken(name string, scopes []string) (Token, string) {
	secret := GenerateSecret()
	return Token{
		ID:        newTokenID(),
		Name:      name,
		TokenHash: HashToken(secret),
		Scopes:    scopes,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, secret
}

// TokenNameExists reports whether a token already uses name (case-insensitive).
func (cfg *Config) TokenNameExists(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, t := range cfg.Auth.Tokens {
		if strings.ToLower(t.Name) == n {
			return true
		}
	}
	return false
}

// FindTokenByHash returns the token whose stored hash matches, or nil.
func (cfg *Config) FindTokenByHash(hash string) *Token {
	for i := range cfg.Auth.Tokens {
		if subtle.ConstantTimeCompare([]byte(cfg.Auth.Tokens[i].TokenHash), []byte(hash)) == 1 {
			return &cfg.Auth.Tokens[i]
		}
	}
	return nil
}

// findTokenIndex returns the index of the token with the given id, or -1.
func (cfg *Config) findTokenIndex(id string) int {
	for i := range cfg.Auth.Tokens {
		if cfg.Auth.Tokens[i].ID == id {
			return i
		}
	}
	return -1
}

// RemoveToken drops the token with the given id, reporting whether it existed.
func (cfg *Config) RemoveToken(id string) bool {
	i := cfg.findTokenIndex(id)
	if i < 0 {
		return false
	}
	cfg.Auth.Tokens = append(cfg.Auth.Tokens[:i], cfg.Auth.Tokens[i+1:]...)
	return true
}

// SetTokenScopes replaces a token's scopes, reporting whether it existed.
func (cfg *Config) SetTokenScopes(id string, scopes []string) bool {
	i := cfg.findTokenIndex(id)
	if i < 0 {
		return false
	}
	cfg.Auth.Tokens[i].Scopes = scopes
	return true
}

type UIConfig struct {
	PageSize     int    `toml:"page_size"`
	ThumbnailFit string `toml:"thumbnail_fit"` // "natural" (real aspect ratio) | "square" (cropped)
}

// LogConfig controls log verbosity: "warn" (default), "info", "debug".
type LogConfig struct {
	Level string `toml:"level"`
}

// ScheduleConfig drives the once-per-day maintenance run at HH:MM on
// every configured gallery.
type ScheduleConfig struct {
	Time              string `toml:"time"` // "HH:MM" 24h, default "01:00"
	SyncGallery       bool   `toml:"sync_gallery"`
	RemoveOrphans     bool   `toml:"remove_orphans"`
	RunAutoTaggers    bool   `toml:"run_auto_taggers"`
	FindRelationPairs bool   `toml:"find_relation_pairs"`
	// The two hash-lookup phases over the images with no source (§7.13).
	// LookupPTR reads monloader's local index in batches and costs nothing;
	// LookupBooru spends monloader's daily budget on the online walk.
	LookupPTR   bool `toml:"lookup_ptr"`
	LookupBooru bool `toml:"lookup_booru"`
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
			MaxFileSizeMB: 2048,
		},
		Tagger: TaggerConfig{
			ExecutionProvider:       defaultExecutionProvider,
			Parallel:                4,
			IdleReleaseAfterMinutes: 15,
			Aggregation:             TaggerAggregationCfg{MinHitFraction: 0.05},
		},
		Auth: AuthConfig{
			SessionLifetimeDays: defaultSessionLifetimeDays,
		},
		UI: UIConfig{
			PageSize:     defaultPageSize,
			ThumbnailFit: defaultThumbnailFit,
		},
		Log: LogConfig{
			Level: "warn",
		},
		Schedule: ScheduleConfig{
			Time:              defaultScheduleTime,
			SyncGallery:       true,
			RemoveOrphans:     true,
			RunAutoTaggers:    false,
			FindRelationPairs: false,
		},
		Relations: RelationsConfig{
			DefaultDistance:     4,
			DefaultSessionOrder: "smallest_distance_first",
			IncrementalOnIngest: true,
			TagPairs:            true,
			TagPairThreshold:    DefaultTagPairThreshold,
		},
	}
}

var scheduleTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// ValidateScheduleTime accepts "HH:MM" in 24-hour form.
func ValidateScheduleTime(v string) error {
	if !scheduleTimeRe.MatchString(v) {
		return fmt.Errorf("schedule.time %q must be HH:MM (00:00-23:59)", v)
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
		// Cleared so a file that omits the key is distinguishable from an
		// explicit "cpu"; the use_cuda migration fires only on the empty value.
		cfg.Tagger.ExecutionProvider = ""
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
	}

	migrateTaggerProvider(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	fillDerivedPaths(cfg)
	applyEnvOverrides(cfg)
	// Re-validate so env overrides ride the same clamps as the TOML path
	// (e.g. a 0/negative session lifetime doesn't slip past the clamp).
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// migrateTaggerProvider maps the legacy use_cuda toggle onto the new
// execution_provider field. A config that sets use_cuda=true without an
// explicit execution_provider is interpreted as "cuda"; otherwise the
// provider defaults to "cpu". The legacy field is cleared so it is never
// serialized back to disk.
func migrateTaggerProvider(cfg *Config) {
	if cfg.Tagger.ExecutionProvider == "" {
		if cfg.Tagger.UseCUDA {
			cfg.Tagger.ExecutionProvider = "cuda"
		} else {
			cfg.Tagger.ExecutionProvider = "cpu"
		}
	}
	cfg.Tagger.UseCUDA = false
}

// Save marshals cfg to TOML and writes atomically to path.
func Save(cfg *Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	// The rename is atomic for the directory entry, not for the bytes
	// behind it. Without the flush a crash in the window leaves a
	// truncated monbooru.toml where the old one was, and Load refuses to
	// parse it - taking the gallery registry, the password hash and every
	// API token with it. A thumbnail regenerates; this does not.
	if err := fsx.WriteAtomic(path, ".monbooru.toml.*", func(f *os.File) error {
		if err := toml.NewEncoder(f).Encode(cfg); err != nil {
			return fmt.Errorf("encoding config: %w", err)
		}
		return f.Sync()
	}); err != nil {
		return err
	}
	fsx.SyncDir(dir)
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

// envParse reads an override, keeping cur on an empty var and warning
// (rather than silently keeping cur) on an unparseable value.
func envParse[T any](key string, cur T, parse func(string) (T, error)) T {
	v := os.Getenv(key)
	if v == "" {
		return cur
	}
	parsed, err := parse(v)
	if err != nil {
		logx.Warnf("config: ignoring %s=%q: %v", key, v, err)
		return cur
	}
	return parsed
}

func envInt(key string, cur int) int { return envParse(key, cur, strconv.Atoi) }

func envBool(key string, cur bool) bool { return envParse(key, cur, strconv.ParseBool) }

// envStr returns the override value, or cur when the var is unset/empty.
func envStr(key, cur string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return cur
}

func applyEnvOverrides(cfg *Config) {
	cfg.Server.BindAddress = envStr("MONBOORU_SERVER_BIND_ADDRESS", cfg.Server.BindAddress)
	cfg.Server.BaseURL = envStr("MONBOORU_SERVER_BASE_URL", cfg.Server.BaseURL)
	cfg.Server.MonloaderURL = envStr("MONBOORU_SERVER_MONLOADER_URL", cfg.Server.MonloaderURL)
	// DATA_PATH stays inline: setting it must also recompute the derived paths.
	if v := os.Getenv("MONBOORU_PATHS_DATA_PATH"); v != "" {
		cfg.Paths.DataPath = v
		fillDerivedPaths(cfg)
	}
	cfg.Paths.ModelPath = envStr("MONBOORU_PATHS_MODEL_PATH", cfg.Paths.ModelPath)
	cfg.Gallery.WatchEnabled = envBool("MONBOORU_GALLERY_WATCH_ENABLED", cfg.Gallery.WatchEnabled)
	cfg.Gallery.MaxFileSizeMB = envInt("MONBOORU_GALLERY_MAX_FILE_SIZE_MB", cfg.Gallery.MaxFileSizeMB)
	cfg.Tagger.ExecutionProvider = envStr("MONBOORU_TAGGER_EXECUTION_PROVIDER", cfg.Tagger.ExecutionProvider)
	// Legacy GPU toggle, kept so existing deployments that export it don't
	// silently fall back to CPU. The provider variable wins when both are set.
	if os.Getenv("MONBOORU_TAGGER_EXECUTION_PROVIDER") == "" && envBool("MONBOORU_TAGGER_USE_CUDA", false) {
		cfg.Tagger.ExecutionProvider = "cuda"
	}
	cfg.Auth.EnablePassword = envBool("MONBOORU_AUTH_ENABLE_PASSWORD", cfg.Auth.EnablePassword)
	cfg.Auth.PasswordHash = envStr("MONBOORU_AUTH_PASSWORD_HASH", cfg.Auth.PasswordHash)
	cfg.Auth.SessionLifetimeDays = envInt("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", cfg.Auth.SessionLifetimeDays)
	cfg.Monloader.APIURL = envStr("MONBOORU_MONLOADER_API_URL", cfg.Monloader.APIURL)
	cfg.Monloader.APIToken = envStr("MONBOORU_MONLOADER_API_TOKEN", cfg.Monloader.APIToken)
	cfg.Log.Level = envStr("MONBOORU_LOG_LEVEL", cfg.Log.Level)
}

// MaxPageSize caps UI.PageSize. A gallery page binds one SQL variable per
// row (the cached-id projection, the API tag/alias loaders); this keeps a
// single page well under SQLite's SQLITE_MAX_VARIABLE_NUMBER (32766).
const MaxPageSize = 1000

// The defaults both Default() and validate() have to agree on: one
// states them for a fresh config, the other snaps a hand-edited TOML
// back onto them.
const (
	defaultScheduleTime        = "01:00"
	defaultExecutionProvider   = "cpu"
	defaultPageSize            = 40
	defaultThumbnailFit        = "natural"
	defaultSessionLifetimeDays = 7
)

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
	if cfg.Auth.EnablePassword {
		h := strings.TrimSpace(cfg.Auth.PasswordHash)
		if !strings.HasPrefix(h, "$2a$") && !strings.HasPrefix(h, "$2b$") && !strings.HasPrefix(h, "$2y$") {
			return fmt.Errorf("auth.password_hash does not look like a bcrypt hash - " +
				"run `monbooru -hash-password 'your-password'` and paste the result into monbooru.toml")
		}
	}
	if len(cfg.Galleries) == 0 {
		return fmt.Errorf("at least one gallery must be configured")
	}
	if cfg.Paths.DataPath == "" {
		return fmt.Errorf("paths.data_path must not be empty")
	}
	seen := map[string]bool{}
	for i := range cfg.Galleries {
		g := &cfg.Galleries[i]
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
		// Everything downstream measures paths against what the filesystem
		// hands back, which is always cleaned and natively separated. On
		// Windows a TOML-friendly "C:/pics" would otherwise never match.
		g.GalleryPath = filepath.Clean(g.GalleryPath)
	}
	if cfg.DefaultGallery == "" {
		cfg.DefaultGallery = cfg.Galleries[0].Name
	} else if cfg.FindGallery(cfg.DefaultGallery) == nil {
		cfg.DefaultGallery = cfg.Galleries[0].Name
	}
	if cfg.Schedule.Time == "" {
		cfg.Schedule.Time = defaultScheduleTime
	} else if err := ValidateScheduleTime(cfg.Schedule.Time); err != nil {
		return err
	}
	if cfg.Tagger.ExecutionProvider == "" {
		cfg.Tagger.ExecutionProvider = defaultExecutionProvider
	} else if !IsValidExecutionProvider(cfg.Tagger.ExecutionProvider) {
		return fmt.Errorf("tagger.execution_provider %q must be one of %v", cfg.Tagger.ExecutionProvider, ValidExecutionProviders)
	}
	// PageSize must be positive: the API path divides by it
	// (offset/limit) and would panic on zero. Snap to the documented
	// default rather than surface a startup error for a user-fixable
	// config typo, and cap it so a page's IN-clause can't overflow the
	// SQL variable limit.
	if cfg.UI.PageSize <= 0 {
		cfg.UI.PageSize = defaultPageSize
	} else if cfg.UI.PageSize > MaxPageSize {
		cfg.UI.PageSize = MaxPageSize
	}
	// ThumbnailFit gates the gallery grid CSS; an unknown value would
	// leave the template class blank. Snap to the default.
	if cfg.UI.ThumbnailFit != "square" {
		cfg.UI.ThumbnailFit = defaultThumbnailFit
	}
	// MaxAge=0 in net/http means "session cookie", so a hand-edited TOML
	// with session_lifetime_days = 0 would expire the user's session at
	// every browser close instead of after the documented 7 days.
	if cfg.Auth.SessionLifetimeDays <= 0 {
		cfg.Auth.SessionLifetimeDays = defaultSessionLifetimeDays
	}
	dropInvalidPlugins(cfg)
	return nil
}

// dropInvalidPlugins warns about and discards hand-written [[plugin]] blocks
// and buttons monbooru could not render, rather than refusing to start over
// a typo in an optional block.
func dropInvalidPlugins(cfg *Config) {
	cfg.Plugins = slices.DeleteFunc(cfg.Plugins, func(p PluginConfig) bool {
		if err := ValidatePluginName(p.Name); err != nil {
			logx.Warnf("config: dropping a [[plugin]] block: %v", err)
			return true
		}
		return false
	})
	for i := range cfg.Plugins {
		p := &cfg.Plugins[i]
		p.Buttons = slices.DeleteFunc(p.Buttons, func(b PluginButton) bool {
			if err := ValidatePluginButton(b); err != nil {
				logx.Warnf("config: dropping a button on plugin %q: %v", p.Name, err)
				return true
			}
			return false
		})
	}
}
