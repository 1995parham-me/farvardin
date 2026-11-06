// Package config loads farvardin's configuration: defaults, then a TOML
// file, then FARVARDIN_* environment variables ("__" separates levels, as
// in FARVARDIN_SERVER__LISTEN).
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

// Config is the whole configuration.
type Config struct {
	Server    Server    `json:"server"    koanf:"server"`
	Store     Store     `json:"store"     koanf:"store"`
	Auth      Auth      `json:"auth"      koanf:"auth"`
	Contacts  Contacts  `json:"contacts"  koanf:"contacts"`
	Calendars Calendars `json:"calendars" koanf:"calendars"`
	Push      Push      `json:"push"      koanf:"push"`
	UI        UI        `json:"ui"        koanf:"ui"`
	// MCP serves the contacts and calendars to assistants.
	MCP MCP `json:"mcp" koanf:"mcp"`
	// Performance holds the knobs for small devices.
	Performance Performance `json:"performance" koanf:"performance"`
	Log         Log         `json:"log"         koanf:"log"`
}

// MCP configures the Model Context Protocol endpoint at /mcp.
type MCP struct {
	Enabled bool `json:"enabled" koanf:"enabled"`
	// Tokens maps a name (written into each commit) to the SHA-256 hex of
	// a bearer token; `farvardin mcp-token NAME` makes one.
	Tokens map[string]string `json:"-" koanf:"tokens"`
}

// Performance bounds memory and CPU. The defaults suit a Raspberry Pi 4;
// see configs/config.example.toml for a 256-512 MB board.
type Performance struct {
	// MemoryLimitMB sets Go's soft memory limit (like GOMEMLIMIT); 0 = off.
	MemoryLimitMB int `json:"memory_limit_mb" koanf:"memory_limit_mb"`
	// ObjectCacheMB is go-git's decoded-object cache per collection.
	ObjectCacheMB int `json:"object_cache_mb" koanf:"object_cache_mb"`
	// ParseCacheEntries bounds the parsed-object cache (~2 KB per card).
	ParseCacheEntries int `json:"parse_cache_entries" koanf:"parse_cache_entries"`
	// ThumbnailCacheMB bounds the scaled-photo cache of the UI.
	ThumbnailCacheMB int `json:"thumbnail_cache_mb" koanf:"thumbnail_cache_mb"`
	// MaxImagePixels refuses to decode larger images (decompression bombs).
	MaxImagePixels int64 `json:"max_image_pixels" koanf:"max_image_pixels"`
	// Warm builds the history index and avatar cache in the background,
	// WarmDelay after start, pausing WarmPause between avatars.
	Warm      bool          `json:"warm"       koanf:"warm"`
	WarmDelay time.Duration `json:"warm_delay" koanf:"warm_delay"`
	WarmPause time.Duration `json:"warm_pause" koanf:"warm_pause"`
	// MaxMultigetHrefs refuses multiget reports asking for more objects.
	MaxMultigetHrefs int `json:"max_multiget_hrefs" koanf:"max_multiget_hrefs"`
}

// UI configures the web UI's own login.
type UI struct {
	// Auth is "proxy" (the [auth] mode, or a proxy in front, guards /ui
	// like the DAV tree) or "jwt" (a login page and a signed session cookie).
	Auth string `json:"auth" koanf:"auth"`
	// Users maps user names to bcrypt hashes for "jwt".
	Users map[string]string `json:"-" koanf:"users"`
	// SessionTTL is how long a login lasts.
	SessionTTL time.Duration `json:"session_ttl" koanf:"session_ttl"`
	// SecretFile holds the session signing key; created when missing.
	SecretFile string `json:"secret_file" koanf:"secret_file"`
	// SecureCookie sends the cookie over HTTPS only. Turn it off only for
	// plain-HTTP testing.
	SecureCookie bool `json:"secure_cookie" koanf:"secure_cookie"`
}

// Push configures WebDAV-Push.
type Push struct {
	Enabled bool `json:"enabled" koanf:"enabled"`
	// StateDir holds vapid/private.pem and the subscription index; with a
	// Xandikos store it is Xandikos's --state-dir, so subscriptions carry over.
	StateDir string `json:"state_dir" koanf:"state_dir"`
	// Subject is the VAPID contact, a mailto: or https: URI.
	Subject string `json:"subject" koanf:"subject"`
}

// Server is the HTTP listener.
type Server struct {
	Listen string `json:"listen" koanf:"listen"`
	// Prefix mounts the DAV tree below a path, for a reverse proxy that
	// serves farvardin under a sub-path. "" serves it at the root.
	Prefix  string `json:"prefix"  koanf:"prefix"`
	UI      bool   `json:"ui"      koanf:"ui"`
	Metrics bool   `json:"metrics" koanf:"metrics"`
	// MetricsListen serves /metrics on its own address (for example
	// 127.0.0.1:8001); empty serves it on Listen.
	MetricsListen string `json:"metrics_listen" koanf:"metrics_listen"`
}

// Store is the git store.
type Store struct {
	Root        string `json:"root"         koanf:"root"`
	Principal   string `json:"principal"    koanf:"principal"`
	AuthorName  string `json:"author_name"  koanf:"author_name"`
	AuthorEmail string `json:"author_email" koanf:"author_email"`
	// Init creates the principal and a default address book and calendar
	// when they do not exist.
	Init bool `json:"init" koanf:"init"`
}

// Auth selects how requests are authenticated.
type Auth struct {
	// Mode is "none" (a proxy in front does it), "basic" (bcrypt users
	// below) or "header" (trust the user name a proxy puts in Header).
	Mode   string            `json:"mode"   koanf:"mode"`
	Realm  string            `json:"realm"  koanf:"realm"`
	Header string            `json:"header" koanf:"header"`
	Users  map[string]string `json:"-"      koanf:"users"`
}

// Contacts holds the address book behaviour.
type Contacts struct {
	MaxResourceSize int64 `json:"max_resource_size" koanf:"max_resource_size"`
	PhotoMaxSize    int   `json:"photo_max_size"    koanf:"photo_max_size"`
	PhotoGuard      bool  `json:"photo_guard"       koanf:"photo_guard"`
	PhotoStabilize  bool  `json:"photo_stabilize"   koanf:"photo_stabilize"`
	ThumbnailSize   int   `json:"thumbnail_size"    koanf:"thumbnail_size"`
	PhotoQuality    int   `json:"photo_quality"     koanf:"photo_quality"`
	// PhoneRegion is the country (ISO 3166-1, "IR") of phone numbers
	// written without a country code. Lint suggests their E.164 form and
	// the UI and MCP can fix them; "" checks only international numbers.
	PhoneRegion string `json:"phone_region" koanf:"phone_region"`
}

// Calendars holds the calendar behaviour.
type Calendars struct {
	Birthdays         bool   `json:"birthdays"           koanf:"birthdays"`
	BirthdayAlarmDays int    `json:"birthday_alarm_days" koanf:"birthday_alarm_days"`
	BirthdayColor     string `json:"birthday_color"      koanf:"birthday_color"`
	UserAddress       string `json:"user_address"        koanf:"user_address"`
}

// Log configures logging.
type Log struct {
	Level  string `json:"level"  koanf:"level"`
	Format string `json:"format" koanf:"format"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Server: Server{Listen: "127.0.0.1:8000", Prefix: "", UI: true, Metrics: true, MetricsListen: ""},
		Store:  Store{Root: "/var/lib/farvardin", Principal: "user", AuthorName: "farvardin", AuthorEmail: "farvardin@localhost", Init: true},
		Auth:   Auth{Mode: "none", Realm: "farvardin", Header: "X-Remote-User", Users: nil},
		Contacts: Contacts{
			MaxResourceSize: 10 << 20, PhotoMaxSize: 720, PhotoGuard: true, PhotoStabilize: true,
			ThumbnailSize: 128, PhotoQuality: 85, PhoneRegion: "",
		},
		Calendars: Calendars{Birthdays: true, BirthdayAlarmDays: 0, BirthdayColor: "#FF2D55", UserAddress: ""},
		Push:      Push{Enabled: false, StateDir: "/var/lib/farvardin/state", Subject: "mailto:farvardin@localhost"},
		UI:        UI{Auth: "proxy", Users: nil, SessionTTL: 7 * 24 * time.Hour, SecretFile: "/var/lib/farvardin/ui-session.key", SecureCookie: true}, //nolint:gosec // a file path, not a credential
		Performance: Performance{
			MemoryLimitMB: 0, ObjectCacheMB: 8, ParseCacheEntries: 4096, ThumbnailCacheMB: 16,
			MaxImagePixels: 16_000_000, Warm: true, WarmDelay: 20 * time.Second, WarmPause: 0, MaxMultigetHrefs: 2000,
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// ErrInvalid is returned for a configuration that cannot work.
var ErrInvalid = errors.New("invalid configuration")

// Load reads defaults, then path (if not empty), then the environment.
func Load(path string) (Config, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Default(), "koanf"), nil); err != nil {
		return Config{}, fmt.Errorf("loading defaults: %w", err)
	}

	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return Config{}, fmt.Errorf("config file: %w", err)
		}

		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return Config{}, fmt.Errorf("loading %s: %w", path, err)
		}
	}

	//nolint:exhaustruct // only the prefix and key mapping are needed
	err := k.Load(env.Provider(".", env.Opt{
		Prefix: "FARVARDIN_",
		TransformFunc: func(key, value string) (string, any) {
			return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(key, "FARVARDIN_")), "__", "."), value
		},
	}), nil)
	if err != nil {
		return Config{}, fmt.Errorf("loading environment: %w", err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return Config{}, fmt.Errorf("decoding configuration: %w", err)
	}

	return cfg, cfg.Validate()
}

// Validate checks values that would fail later in a confusing way.
func (c Config) Validate() error {
	switch c.Auth.Mode {
	case "none", "header":
	case "basic":
		if len(c.Auth.Users) == 0 {
			return fmt.Errorf("%w: auth.mode = basic needs at least one [auth.users] entry", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: auth.mode must be none, basic or header, not %q", ErrInvalid, c.Auth.Mode)
	}

	if c.Store.Principal == "" || strings.ContainsAny(c.Store.Principal, "/.") || c.Store.Principal == "ui" || c.Store.Principal == "metrics" {
		return fmt.Errorf("%w: store.principal %q is not usable", ErrInvalid, c.Store.Principal)
	}

	if c.MCP.Enabled && len(c.MCP.Tokens) == 0 {
		return fmt.Errorf("%w: mcp.enabled needs at least one [mcp.tokens] entry", ErrInvalid)
	}

	switch c.UI.Auth {
	case "proxy":
	case "jwt":
		if len(c.UI.Users) == 0 {
			return fmt.Errorf("%w: ui.auth = jwt needs at least one [ui.users] entry", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: ui.auth must be proxy or jwt, not %q", ErrInvalid, c.UI.Auth)
	}

	if c.Contacts.PhoneRegion != "" && !contact.ValidRegion(c.Contacts.PhoneRegion) {
		return fmt.Errorf("%w: contacts.phone_region %q is not a known country code (ISO 3166-1, like \"IR\")", ErrInvalid, c.Contacts.PhoneRegion)
	}

	if c.Store.Root == "" {
		return fmt.Errorf("%w: store.root is empty", ErrInvalid)
	}

	return nil
}
