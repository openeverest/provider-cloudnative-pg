package main

import (
	"strings"
	"testing"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestMarkdown(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "escapes HTML and emphasis outside code spans",
			in:   "creates <instance>-metrics when a pgaudit.* parameter is set, see `a <b> *c*`",
			want: "creates &lt;instance&gt;-metrics when a pgaudit.\\* parameter is set, see `a <b> *c*`",
		},
		{
			name: "indented lines after a blank line are code",
			in:   "records such as\n\n  hostssl db user 10.0.0.0/8 scram-sha-256\n  host    db all  all        reject\n\nThen # comments.",
			want: "records such as\n\n```text\nhostssl db user 10.0.0.0/8 scram-sha-256\nhost    db all  all        reject\n```\n\nThen # comments.",
		},
		{
			name: "bullets with continuations and nested bullets, ended by a plain line",
			in:   "Differences:\n* first\n  continued\n* names:\n  * 'a' - x\n  * 'b' - y\nAfter.",
			want: "Differences:\n- first\n  continued\n- names:\n  - 'a' - x\n  - 'b' - y\n\nAfter.",
		},
		{
			name: "a leading # is not a heading",
			in:   "# comments are ignored",
			want: "\\# comments are ignored",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markdown(tt.in); got != tt.want {
				t.Errorf("markdown() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestTypeName(t *testing.T) {
	str := &apiextensionsv1.JSONSchemaProps{Type: "string"}
	tests := []struct {
		name string
		in   apiextensionsv1.JSONSchemaProps
		want string
	}{
		{"array", apiextensionsv1.JSONSchemaProps{Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: str}}, "[]string"},
		{"map", apiextensionsv1.JSONSchemaProps{Type: "object", AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{Schema: str}}, "map[string]string"},
		{"int or string", apiextensionsv1.JSONSchemaProps{XIntOrString: true}, "integer or string"},
		{"format", apiextensionsv1.JSONSchemaProps{Type: "string", Format: "date-time"}, "string (date-time)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := typeName(&tt.in); got != tt.want {
				t.Errorf("typeName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func provider(engine *apiextensionsv1.JSONSchemaProps) *corev1alpha1.Provider {
	p := &corev1alpha1.Provider{}
	p.Name = "provider-test"
	p.Spec.Components = map[string]corev1alpha1.Component{
		"engine": {Type: "postgresql", ParametersSchema: &common.ParametersSchema{OpenAPIV3Schema: engine}},
	}
	return p
}

func TestRender(t *testing.T) {
	doc, err := render(provider(&apiextensionsv1.JSONSchemaProps{
		Type:     "object",
		Required: []string{"mode"},
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"mode": {Type: "string", Description: "Mode of the pool.", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"session"`)}, {Raw: []byte(`"transaction"`)}}},
			"roles": {Type: "array", Description: "Roles to manage.", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
				Type:       "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{"name": {Type: "string", Description: "Name of the role."}},
			}}},
		},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# provider-test reference",
		"## Component engine (postgresql)\n\nSet in `spec.components.engine.parameters` of an Instance.",
		"### `mode`\n\n`string` · required · one of `session`, `transaction`\n\nMode of the pool.",
		"#### `roles[].name`\n\n`string`\n\nName of the role.",
	} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("missing %q in\n%s", want, doc)
		}
	}
}

func TestRenderRejectsUndocumentedFields(t *testing.T) {
	_, err := render(provider(&apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"bootstrap": {Type: "object", Description: "How it starts.", Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"name": {Type: "string"},
			}},
		},
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "Component engine (postgresql): bootstrap.name") {
		t.Fatalf("render() error = %v, want one naming bootstrap.name", err)
	}
}
