package flags

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// envDefaults holds the fallback value, in string form, for each environment
// variable, applied when the variable is absent from the process environment.
// Populated by SetDefaults; mirrors the former viper.SetDefault layer.
var envDefaults = map[string]string{}

// envKeys records every environment variable the flag layer reads so that the
// documentation-completeness test can enumerate them. Replaces viper.AllKeys.
var envKeys = map[string]struct{}{}

func registerEnvKey(key string) { envKeys[key] = struct{}{} }

// AllEnvKeys returns every environment variable bound by the flag layer,
// sorted. Replaces viper.AllKeys, which the docs-completeness test relies on to
// assert that every variable watchtower reads is documented.
func AllEnvKeys() []string {
	keys := make([]string, 0, len(envKeys))
	for k := range envKeys {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// lookupEnv returns the effective value for key: the process environment when
// set, otherwise the registered default. The key is recorded for AllEnvKeys.
func lookupEnv(key string) (string, bool) {
	registerEnvKey(key)

	if v, ok := os.LookupEnv(key); ok {
		return v, true
	}

	v, ok := envDefaults[key]

	return v, ok
}

func envString(key string) string {
	v, _ := lookupEnv(key)

	return v
}

// envStringSlice splits a single environment value on whitespace, matching the
// previous viper.GetStringSlice behavior (which, per spf13/viper#380, does not
// split on commas). Returns an empty slice when the value is unset or blank.
func envStringSlice(key string) []string {
	v, ok := lookupEnv(key)
	if !ok || v == "" {
		return []string{}
	}

	return strings.Fields(v)
}

func envInt(key string) int {
	v, _ := lookupEnv(key)
	n, _ := strconv.Atoi(v)

	return n
}

func envBool(key string) bool {
	v, ok := lookupEnv(key)
	if !ok {
		return false
	}

	b, _ := strconv.ParseBool(v)

	return b
}

func envDuration(key string, fallback ...time.Duration) time.Duration {
	v, ok := lookupEnv(key)
	if !ok || v == "" {
		if len(fallback) > 0 {
			return fallback[0]
		}

		return 0
	}

	d := parseDuration(v)
	if d == 0 && len(fallback) > 0 {
		return fallback[0]
	}

	return d
}

// parseDuration mirrors spf13/cast.ToDuration's string handling: a value
// carrying a unit character is parsed as a Go duration, while a bare number is
// interpreted as nanoseconds. An unparseable value yields zero.
func parseDuration(s string) time.Duration {
	if strings.ContainsAny(s, "nsuµmh") {
		d, _ := time.ParseDuration(s)

		return d
	}

	d, _ := time.ParseDuration(s + "ns")

	return d
}

// envIsSet reports whether key is present in the process environment,
// regardless of value. It deliberately does not register the key: the previous
// viper.IsSet path read NO_COLOR via AutomaticEnv, which never appeared in
// viper.AllKeys, so it must stay out of AllEnvKeys too.
func envIsSet(key string) bool {
	_, ok := os.LookupEnv(key)

	return ok
}
