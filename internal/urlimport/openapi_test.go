package urlimport

import (
	"strings"
	"testing"
)

func TestConvertOpenAPI_Petstore(t *testing.T) {
	spec := loadFixture(t, "petstore-openapi.yaml")
	res, err := ConvertOpenAPI(spec, "https://petstore.example.com/openapi.yaml")
	if err != nil {
		t.Fatalf("ConvertOpenAPI: %v", err)
	}
	if res.Title != "Swagger Petstore" {
		t.Errorf("Title = %q, want 'Swagger Petstore'", res.Title)
	}
	md := res.Markdown

	wantSubstrings := []string{
		// Info block
		"# Swagger Petstore",
		"**Version:** `1.0.0`",
		"**Contact:**",
		"petstore@example.com",
		"**License:**",
		"MIT",
		// Servers
		"## Servers",
		"https://petstore.example.com/v1",
		"Production server",
		// Endpoints
		"## Endpoints",
		"### pets",
		"### store",
		"`GET /pets`",
		"`POST /pets`",
		"`GET /pets/{petId}`",
		"`POST /store/orders`",
		// Operation details
		"List pets",
		"Create a pet",
		"**Parameters**",
		"| `limit` | query | no |",
		"| `petId` | path | yes |",
		// Request body
		"**Request body** *(required)*",
		"Content-Type: `application/json`",
		// Example body
		"name: Rex",
		// Responses
		"**Responses**",
		"| `200` |",
		"| `400` |",
		"| `404` |",
		"| `201` |",
		// Deprecation marker
		"⚠ **Deprecated.**",
		// Operation IDs
		"**Operation ID:** `listPets`",
		// Schemas
		"## Schemas",
		"### `Order`",
		"### `Pet`",
		"A pet for sale.",
		"| `id` |",
		"| `name` |",
		"`integer (int64)`",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n--- markdown ---\n%s\n---", want, md)
		}
	}
}

func TestConvertOpenAPI_RejectsSwagger2(t *testing.T) {
	// libopenapi accepts both v2 and v3 documents. ConvertOpenAPI is
	// limited to 3.x; Swagger 2.0 should produce a clear error so the
	// caller can fall back to generic conversion.
	swagger2 := []byte(`swagger: "2.0"
info:
  title: Old API
  version: "1.0"
paths: {}
`)
	_, err := ConvertOpenAPI(swagger2, "")
	if err == nil {
		t.Fatal("ConvertOpenAPI: expected error for Swagger 2.0, got nil")
	}
	if !strings.Contains(err.Error(), "only 3.x") {
		t.Fatalf("error = %v, want to mention 'only 3.x'", err)
	}
}

func TestConvertOpenAPI_RejectsGarbage(t *testing.T) {
	_, err := ConvertOpenAPI([]byte("not an openapi spec"), "")
	if err == nil {
		t.Fatal("expected error for non-OpenAPI input, got nil")
	}
}

func TestConvertOpenAPI_MinimalSpec(t *testing.T) {
	// Smallest valid v3 spec — just info and an empty paths object.
	minimal := []byte(`openapi: 3.0.0
info:
  title: Tiny API
  version: "0.1"
paths: {}
`)
	res, err := ConvertOpenAPI(minimal, "")
	if err != nil {
		t.Fatalf("ConvertOpenAPI: %v", err)
	}
	if !strings.Contains(res.Markdown, "# Tiny API") {
		t.Errorf("markdown missing title heading\n%s", res.Markdown)
	}
	if strings.Contains(res.Markdown, "## Endpoints") {
		t.Errorf("markdown should not have Endpoints section for empty paths\n%s", res.Markdown)
	}
}

func TestConvertOpenAPI_PathLevelParametersMerged(t *testing.T) {
	// Path-level parameters apply to every operation on that path.
	// Operation-level parameters override path-level ones on a
	// (name, in) match. Both rules must hold.
	spec := []byte(`openapi: 3.0.0
info:
  title: Param Merge Test
  version: "1.0"
paths:
  /widgets/{widgetId}:
    parameters:
      - name: widgetId
        in: path
        required: true
        description: Path-level widget id
        schema:
          type: string
      - name: trace
        in: header
        required: false
        description: Trace header from path-level
        schema:
          type: string
    get:
      summary: Get a widget
      parameters:
        - name: trace
          in: header
          required: true
          description: Operation-level trace (overrides path-level)
          schema:
            type: string
        - name: fields
          in: query
          required: false
          description: Op-only filter
          schema:
            type: string
      responses:
        '200':
          description: ok
`)
	res, err := ConvertOpenAPI(spec, "")
	if err != nil {
		t.Fatalf("ConvertOpenAPI: %v", err)
	}
	md := res.Markdown
	// Path-level parameter survives merge.
	if !strings.Contains(md, "| `widgetId` | path | yes |") {
		t.Errorf("missing path-level widgetId row in:\n%s", md)
	}
	// Operation-level override wins on (name, in) collision: trace
	// goes from required=false (path) to required=yes (op).
	if !strings.Contains(md, "| `trace` | header | yes |") {
		t.Errorf("op-level trace override missing or didn't win in:\n%s", md)
	}
	if strings.Contains(md, "| `trace` | header | no |") {
		t.Errorf("path-level trace row leaked in; op-level should override:\n%s", md)
	}
	// Op-only parameter present.
	if !strings.Contains(md, "| `fields` | query |") {
		t.Errorf("missing op-only fields row in:\n%s", md)
	}
}

func TestMergeParameters_EmptyInputs(t *testing.T) {
	if got := mergeParameters(nil, nil); got != nil {
		t.Errorf("mergeParameters(nil, nil) = %v, want nil", got)
	}
}

func TestConvertOpenAPI_ArrayOfRefTypeCell(t *testing.T) {
	// Schema properties that are arrays of component refs must render
	// as a single balanced backtick-wrapped type cell. The previous
	// schemaTypeBrief→codeOrBlank pipeline could double-wrap and emit
	// "`array of `Pet``" which breaks the table.
	spec := []byte(`openapi: 3.0.0
info:
  title: Array Of Ref Test
  version: "1.0"
paths: {}
components:
  schemas:
    Pet:
      type: object
      properties:
        name:
          type: string
    Litter:
      type: object
      properties:
        pets:
          type: array
          items:
            $ref: '#/components/schemas/Pet'
`)
	res, err := ConvertOpenAPI(spec, "")
	if err != nil {
		t.Fatalf("ConvertOpenAPI: %v", err)
	}
	want := "| `pets` | `array of Pet` |"
	if !strings.Contains(res.Markdown, want) {
		t.Errorf("missing %q in:\n%s", want, res.Markdown)
	}
	// Negative: no double-backticked nor stray-backticked variants.
	bad := []string{
		"`array of `Pet``",
		"``array",
		"Pet``",
	}
	for _, b := range bad {
		if strings.Contains(res.Markdown, b) {
			t.Errorf("found malformed type cell substring %q in:\n%s", b, res.Markdown)
		}
	}
}

func TestCodeOrBlank(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"string", "`string`"},
		{"  string  ", "`string`"},
		{"array of Pet", "`array of Pet`"},
		{"`already`", "`already`"},
		{"with`backtick`inside", "`withbacktickinside`"},
		{"`", ""},
	}
	for _, tc := range tests {
		if got := codeOrBlank(tc.in); got != tc.want {
			t.Errorf("codeOrBlank(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSchemaTypeBrief_Nil(t *testing.T) {
	if got := schemaTypeBrief(nil); got != "" {
		t.Errorf("schemaTypeBrief(nil) = %q, want empty", got)
	}
}

func TestEscapeTableCell(t *testing.T) {
	tests := []struct{ in, want string }{
		{`plain text`, `plain text`},
		{`with | pipe`, `with \| pipe`},
		{`back\slash`, `back\\slash`},
		{`both | and \`, `both \| and \\`},
		{``, ``},
	}
	for _, tc := range tests {
		if got := escapeTableCell(tc.in); got != tc.want {
			t.Errorf("escapeTableCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSingleLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"single line", "single line"},
		{"multi\nline\ntext", "multi line text"},
		{"  whitespace   collapsed  ", "whitespace collapsed"},
		{"with | pipe", "with \\| pipe"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := singleLine(tc.in); got != tc.want {
			t.Errorf("singleLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
