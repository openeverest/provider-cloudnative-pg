package cnpg

import (
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

func TestParseConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    map[string]string
		wantErr string
	}{
		{name: "empty", text: "", want: map[string]string{}},
		{name: "blank lines and comments", text: "\n  # a comment\n\t\r\n", want: map[string]string{}},
		{
			name: "postgresql.conf syntax",
			text: `
# connections
max_connections = 200   # trailing comment
work_mem 64MB
shared_buffers='512MB'
pgaudit.log = 'ddl, role'
search_path = '"$user", public'
log_line_prefix = 'it''s %m \'x\' '
default_text_search_config = ''
`,
			want: map[string]string{
				"max_connections":            "200",
				"work_mem":                   "64MB",
				"shared_buffers":             "512MB",
				"pgaudit.log":                "ddl, role",
				"search_path":                `"$user", public`,
				"log_line_prefix":            "it's %m 'x' ",
				"default_text_search_config": "",
			},
		},
		{name: "windows line endings", text: "max_connections = 200\r\nwork_mem = 4MB\r\n", want: map[string]string{"max_connections": "200", "work_mem": "4MB"}},
		{name: "no value", text: "max_connections", wantErr: "line 1: \"max_connections\" has no value"},
		{name: "empty value", text: "work_mem = # nothing", wantErr: "work_mem has no value"},
		{name: "invalid name", text: "1max = 2", wantErr: "not a valid parameter name"},
		{name: "two dots", text: "a.b.c = 2", wantErr: "not a valid parameter name"},
		{name: "unquoted blanks", text: "pgaudit.log = ddl, role", wantErr: "quote values that contain blanks"},
		{name: "unclosed quote", text: "pgaudit.log = 'ddl", wantErr: "no closing quote"},
		{name: "set twice", text: "work_mem = 4MB\nwork_mem = 8MB", wantErr: "line 2: work_mem is set more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfiguration(tt.text)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyConfiguration(t *testing.T) {
	t.Run("merges into postgresql.parameters", func(t *testing.T) {
		custom := &components.CNPGCustomSpec{
			Configuration: "max_connections = 200",
			PostgresConfiguration: &cnpgv1.PostgresConfiguration{
				Parameters: map[string]string{"shared_buffers": "128MB"},
			},
		}
		require.NoError(t, ApplyConfiguration(custom))
		assert.Equal(t, map[string]string{"max_connections": "200", "shared_buffers": "128MB"}, custom.PostgresConfiguration.Parameters)
	})

	t.Run("without postgresql", func(t *testing.T) {
		custom := &components.CNPGCustomSpec{Configuration: "max_connections = 200"}
		require.NoError(t, ApplyConfiguration(custom))
		assert.Equal(t, map[string]string{"max_connections": "200"}, custom.PostgresConfiguration.Parameters)
	})

	t.Run("comments only leave postgresql unset", func(t *testing.T) {
		custom := &components.CNPGCustomSpec{Configuration: "# nothing yet\n"}
		require.NoError(t, ApplyConfiguration(custom))
		assert.Nil(t, custom.PostgresConfiguration)
	})

	t.Run("refuses a parameter set in both", func(t *testing.T) {
		params := map[string]string{"max_connections": "100"}
		custom := &components.CNPGCustomSpec{
			Configuration:         "max_connections = 200",
			PostgresConfiguration: &cnpgv1.PostgresConfiguration{Parameters: params},
		}
		require.ErrorContains(t, ApplyConfiguration(custom), "max_connections is set in both")
		assert.Equal(t, map[string]string{"max_connections": "100"}, params, "the decoded parameters are left alone")
	})
}

func TestValidateParameters(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		wantErr string
	}{
		{name: "none"},
		{name: "tunable parameters", params: map[string]string{"max_connections": "200", "pgaudit.log": "ddl", "shared_buffers": "1GB"}},
		{name: "fixed parameter", params: map[string]string{"port": "6432"}, wantErr: "remove them: port"},
		{name: "blocked parameter", params: map[string]string{"listen_addresses": "*"}, wantErr: "remove them: listen_addresses"},
		{
			name:    "every rejected parameter, sorted",
			params:  map[string]string{"ssl": "off", "archive_mode": "off", "work_mem": "4MB"},
			wantErr: "remove them: archive_mode, ssl",
		},
		{name: "fixed parameter at the value CloudNativePG sets", params: map[string]string{"archive_mode": "on"}},
		{name: "mandatory parameter, as the webhook does", params: map[string]string{"ssl": "on"}, wantErr: "remove them: ssl"},
		{name: "invalid name", params: map[string]string{"max connections": "1"}, wantErr: "not a valid PostgreSQL parameter name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateParameters(tt.params, &components.CNPGCustomSpec{}, "", "")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), tt.wantErr), "error %q lacks %q", err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}
