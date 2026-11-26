package cnpg

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

// parameterName matches a PostgreSQL configuration parameter name, as the
// CloudNativePG webhook does: a GUC name, optionally namespaced with one dot.
var parameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`)

// ParseConfiguration parses the configuration parameter: postgresql.conf
// settings, one "name = value" per line (the "=" is optional), with blank
// lines and # comments ignored. Values may be single-quoted; a quote inside
// is doubled or backslash-escaped. A parameter may be set only once.
func ParseConfiguration(text string) (map[string]string, error) {
	params := map[string]string{}
	for i, raw := range strings.Split(text, "\n") {
		name, value, err := parseConfigurationLine(raw)
		if err != nil {
			return nil, fmt.Errorf("configuration line %d: %w", i+1, err)
		}
		if name == "" {
			continue
		}
		if _, set := params[name]; set {
			return nil, fmt.Errorf("configuration line %d: %s is set more than once", i+1, name)
		}
		params[name] = value
	}
	return params, nil
}

func parseConfigurationLine(raw string) (string, string, error) {
	line := strings.TrimSpace(raw)
	if line == "" || line[0] == '#' {
		return "", "", nil
	}
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		return "", "", fmt.Errorf("%q has no value; write name = value", line)
	}
	name := line[:end]
	if !parameterName.MatchString(name) {
		return "", "", fmt.Errorf("%q is not a valid parameter name", name)
	}
	rest := strings.TrimLeft(line[end:], " \t")
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	if rest == "" || rest[0] == '#' {
		return "", "", fmt.Errorf("%s has no value", name)
	}

	var value string
	if rest[0] == '\'' {
		var ok bool
		if value, rest, ok = unquote(rest); !ok {
			return "", "", fmt.Errorf("the value of %s has no closing quote", name)
		}
	} else {
		end := strings.IndexAny(rest, " \t#")
		if end < 0 {
			end = len(rest)
		}
		value, rest = rest[:end], rest[end:]
	}
	if rest = strings.TrimSpace(rest); rest != "" && rest[0] != '#' {
		return "", "", fmt.Errorf("unexpected %q after the value of %s; quote values that contain blanks", rest, name)
	}
	return name, value, nil
}

// unquote reads the single-quoted string s starts with, and returns its
// value and what follows it.
func unquote(s string) (value, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
		case s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == '\'':
			return b.String(), s[i+1:], true
		}
		b.WriteByte(s[i])
	}
	return "", "", false
}

// ApplyConfiguration merges the configuration parameter into
// postgresql.parameters, so everything that reads the user's parameters sees
// both. A parameter set in both is refused.
func ApplyConfiguration(custom *components.CNPGCustomSpec) error {
	if strings.TrimSpace(custom.Configuration) == "" {
		return nil
	}
	params, err := ParseConfiguration(custom.Configuration)
	if err != nil {
		return err
	}
	if len(params) == 0 {
		return nil
	}
	if custom.PostgresConfiguration == nil {
		custom.PostgresConfiguration = &cnpgv1.PostgresConfiguration{}
	}
	merged := maps.Clone(custom.PostgresConfiguration.Parameters)
	if merged == nil {
		merged = map[string]string{}
	}
	for _, name := range slices.Sorted(maps.Keys(params)) {
		if _, set := merged[name]; set {
			return fmt.Errorf("%s is set in both configuration and postgresql.parameters; keep one", name)
		}
		merged[name] = params[name]
	}
	custom.PostgresConfiguration.Parameters = merged
	return nil
}

// ValidateParameters mirrors the CloudNativePG webhook's checks of
// PostgreSQL parameters: valid names, and no parameter CloudNativePG manages
// itself unless it is set to the value CloudNativePG uses anyway.
func ValidateParameters(params map[string]string, custom *components.CNPGCustomSpec, engineVersion, instanceVersion string) error {
	if len(params) == 0 {
		return nil
	}
	major, _ := strconv.Atoi(postgresMajor(engineVersion, instanceVersion))
	managed := postgres.CreatePostgresqlConfiguration(postgres.ConfigurationInfo{
		Settings:             postgres.CnpgConfigurationSettings,
		MajorVersion:         major,
		UserSettings:         params,
		IsAlterSystemEnabled: custom.PostgresConfiguration != nil && custom.PostgresConfiguration.EnableAlterSystem,
	}).GetConfigurationParameters()

	var rejected []string
	for _, name := range slices.Sorted(maps.Keys(params)) {
		if !parameterName.MatchString(name) {
			return fmt.Errorf("%q is not a valid PostgreSQL parameter name", name)
		}
		if _, fixed := postgres.FixedConfigurationParameters[name]; !fixed {
			continue
		}
		if value, ok := managed[name]; !ok || value != params[name] {
			rejected = append(rejected, name)
		}
	}
	if len(rejected) > 0 {
		return fmt.Errorf("CloudNativePG manages these parameters itself, remove them: %s", strings.Join(rejected, ", "))
	}
	return nil
}
