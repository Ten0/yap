package config

import (
	"os"
	"reflect"
	"slices"
	"strings"
)

// Environment variable names. Exported constants so downstream
// packages (CLI, docs generators) can reference the same source of
// truth and any rename is a compile-time error.
const (
	// EnvAPIKey is the primary transcription API key override.
	EnvAPIKey = "YAP_API_KEY"
	// EnvGroqAPIKey is the legacy alias preserved for compatibility.
	// Only consulted when EnvAPIKey is unset.
	EnvGroqAPIKey = "GROQ_API_KEY"
	// EnvTransformAPIKey is the transform backend API key override.
	EnvTransformAPIKey = "YAP_TRANSFORM_API_KEY"
	// EnvHotkey overrides general.hotkey. Legacy alias from the flat
	// config era; kept for wizard and smoke-test ergonomics.
	EnvHotkey = "YAP_HOTKEY"
	// EnvConfig overrides the config file path. Consumed by
	// internal/config.ConfigPath, not by ApplyEnvOverrides.
	EnvConfig = "YAP_CONFIG"
)

// ApplyEnvOverrides mutates cfg in place with values from environment
// variables. Precedence is env > file > default.
//
//	YAP_API_KEY           -> Transcription.APIKey (primary)
//	GROQ_API_KEY          -> Transcription.APIKey (compat; only if YAP_API_KEY unset)
//	YAP_TRANSFORM_API_KEY -> Transform.APIKey
//	YAP_HOTKEY            -> General.Hotkey (compat)
//
// YAP_CONFIG is not an override of Config itself; it selects the file
// path and is handled in internal/config.ConfigPath.
func ApplyEnvOverrides(cfg *Config) {
	if v := os.Getenv(EnvAPIKey); v != "" {
		cfg.Transcription.APIKey = v
	} else if v := os.Getenv(EnvGroqAPIKey); v != "" {
		cfg.Transcription.APIKey = v
	}
	if v := os.Getenv(EnvTransformAPIKey); v != "" {
		cfg.Transform.APIKey = v
	}
	if v := os.Getenv(EnvHotkey); v != "" {
		cfg.General.Hotkey = v
	}
}

// SecretEnvNames returns the sorted environment variable names that
// carry secrets into the configuration.
//
// It is derived by reflection from the `yap:"secret;env=..."` struct
// tags on Config, so the schema decides: a secret field added later is
// covered the moment it is tagged, without anyone having to remember
// that this function exists.
//
// Callers that hand yap's environment to a child process — see
// pkg/yap/hint/exec — must remove these names first. A helper program
// has no business seeing the user's API keys.
func SecretEnvNames() []string {
	var names []string
	cfgType := reflect.TypeOf(Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		section := cfgType.Field(i).Type
		if section.Kind() != reflect.Struct {
			continue
		}
		for j := 0; j < section.NumField(); j++ {
			for _, name := range secretEnvNamesFromTag(section.Field(j).Tag.Get("yap")) {
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	slices.Sort(names)
	return names
}

// secretEnvNamesFromTag returns the env var names declared by one
// `yap:"..."` tag, or nil when the field is not marked secret. The
// grammar matches internal/cmd/gen-nixos: semicolon-separated parts, a
// bare `secret` flag, `env=` a comma-separated list, and `doc=` greedy
// and therefore last.
func secretEnvNamesFromTag(tag string) []string {
	var names []string
	secret := false
	for rest := tag; rest != ""; {
		if strings.HasPrefix(rest, "doc=") {
			break
		}
		part := rest
		if i := strings.IndexByte(rest, ';'); i >= 0 {
			part, rest = rest[:i], strings.TrimSpace(rest[i+1:])
		} else {
			rest = ""
		}
		switch part = strings.TrimSpace(part); {
		case part == "secret":
			secret = true
		case strings.HasPrefix(part, "env="):
			for _, name := range strings.Split(part[len("env="):], ",") {
				if name = strings.TrimSpace(name); name != "" {
					names = append(names, name)
				}
			}
		}
	}
	if !secret {
		return nil
	}
	return names
}
