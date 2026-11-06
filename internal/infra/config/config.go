// Package config loads farvardin's configuration: defaults, then a TOML
// file, then FARVARDIN_* environment variables ("__" separates levels, as
// in FARVARDIN_SERVER__LISTEN).
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Config is the whole configuration.
type Config struct {
	Server    Server    `json:"server"    koanf:"server"`
	Store     Store     `json:"store"     koanf:"store"`
	Auth      Auth      `json:"auth"      koanf:"auth"`
	Contacts  Contacts  `json:"contacts"  koanf:"contacts"`
	Calendars Calendars `json:"calendars" koanf:"calendars"`
	Log       Log       `json:"log"       koanf:"log"`
}

// Server is the HTTP listener.
type Server struct {
	Listen string `json:"listen" koanf:"listen"`
	// Prefix mounts the DAV tree below a path, for a reverse proxy that
	// serves farvardin under a sub-path. "" serves it at the root.
	Prefix  string `json:"prefix"  koanf:"prefix"`
	UI      bool   `json:"ui"      koanf:"ui"`
	Metrics bool   `json:"metrics" koanf:"metrics"`
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
		Server: Server{Listen: "127.0.0.1:8000", Prefix: "", UI: true, Metrics: true},
		Store:  Store{Root: "/var/lib/farvardin", Principal: "user", AuthorName: "farvardin", AuthorEmail: "farvardin@localhost", Init: true},
		Auth:   Auth{Mode: "none", Realm: "farvardin", Header: "X-Remote-User", Users: nil},
		Contacts: Contacts{
			MaxResourceSize: 10 << 20, PhotoMaxSize: 720, PhotoGuard: true, PhotoStabilize: true,
			ThumbnailSize: 128, PhotoQuality: 85,
		},
		Calendars: Calendars{Birthdays: true, BirthdayAlarmDays: 0, BirthdayColor: "#FF2D55", UserAddress: ""},
		Log:       Log{Level: "info", Format: "text"},
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

	if c.Store.Root == "" {
		return fmt.Errorf("%w: store.root is empty", ErrInvalid)
	}

	return nil
}
