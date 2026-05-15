package urlimport

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"
	"go.yaml.in/yaml/v4"
)

// ConvertOpenAPI converts an OpenAPI 3.x specification into a
// human-readable markdown document. The output is organized as:
//
//   - H1 with the API title
//   - Info section (version, description, license, contact)
//   - One section per tag (or "Other" for untagged operations), with
//     a sub-section per Operation containing method+path, summary,
//     parameter table, request-body summary, and response codes
//   - Schemas section listing component schemas as markdown tables
//
// Swagger 2.0 specifications are detected by the Detect() function
// but are not converted by this function — they return an error so
// the caller can fall back to the generic converter. v3 covers the
// modern surface; v2 → v3 upgrade is a possible follow-up if real
// usage demands it.
func ConvertOpenAPI(spec []byte, pageURL string) (*ConvertResult, error) {
	doc, err := libopenapi.NewDocument(spec)
	if err != nil {
		return nil, fmt.Errorf("urlimport: parse OpenAPI: %w", err)
	}

	version := doc.GetVersion()
	if !strings.HasPrefix(version, "3.") {
		return nil, fmt.Errorf("urlimport: OpenAPI %s not supported (only 3.x)", version)
	}

	model, buildErr := doc.BuildV3Model()
	// libopenapi returns a non-nil error here for unresolved refs and
	// other recoverable issues; when model is nil the error is fatal,
	// otherwise we render what we got. The recoverable error is
	// swallowed because the model is still usable.
	if buildErr != nil && model == nil {
		return nil, fmt.Errorf("urlimport: build OpenAPI model: %w", buildErr)
	}
	if model == nil || model.Model.Info == nil {
		return nil, errors.New("urlimport: OpenAPI document missing required Info block")
	}

	var b strings.Builder
	renderInfo(&b, &model.Model)
	renderServers(&b, &model.Model)
	renderOperations(&b, &model.Model)
	renderSchemas(&b, &model.Model)

	md := cleanupMarkdown(b.String())
	if strings.TrimSpace(md) == "" {
		return nil, errors.New("urlimport: empty markdown after OpenAPI conversion")
	}

	return &ConvertResult{
		Markdown: md,
		Title:    strings.TrimSpace(model.Model.Info.Title),
		Byline:   "",
	}, nil
}

func renderInfo(b *strings.Builder, doc *v3high.Document) {
	info := doc.Info
	title := strings.TrimSpace(info.Title)
	if title == "" {
		title = "API Reference"
	}
	fmt.Fprintf(b, "# %s\n\n", title)
	if v := strings.TrimSpace(info.Version); v != "" {
		fmt.Fprintf(b, "**Version:** `%s`\n\n", v)
	}
	if v := strings.TrimSpace(info.Description); v != "" {
		b.WriteString(v)
		b.WriteString("\n\n")
	}
	if info.Contact != nil {
		var parts []string
		if info.Contact.Name != "" {
			parts = append(parts, info.Contact.Name)
		}
		if info.Contact.Email != "" {
			parts = append(parts, fmt.Sprintf("<%s>", info.Contact.Email))
		}
		if info.Contact.URL != "" {
			parts = append(parts, info.Contact.URL)
		}
		if len(parts) > 0 {
			fmt.Fprintf(b, "**Contact:** %s\n\n", strings.Join(parts, " · "))
		}
	}
	if info.License != nil && info.License.Name != "" {
		license := info.License.Name
		if info.License.URL != "" {
			license = fmt.Sprintf("[%s](%s)", info.License.Name, info.License.URL)
		}
		fmt.Fprintf(b, "**License:** %s\n\n", license)
	}
}

func renderServers(b *strings.Builder, doc *v3high.Document) {
	if len(doc.Servers) == 0 {
		return
	}
	b.WriteString("## Servers\n\n")
	for _, s := range doc.Servers {
		url := strings.TrimSpace(s.URL)
		if url == "" {
			continue
		}
		fmt.Fprintf(b, "- `%s`", url)
		if d := strings.TrimSpace(s.Description); d != "" {
			fmt.Fprintf(b, " — %s", d)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// opSlot is a unit of work for the tag-grouping pass: one HTTP method
// against one path, plus the Operation that defines it. pathParams
// carries the path-item's shared parameters so renderOperation can
// merge them with operation-scoped parameters per the OpenAPI spec
// (operation-level overrides path-level on a (name, in) match).
type opSlot struct {
	method     string
	path       string
	op         *v3high.Operation
	pathParams []*v3high.Parameter
}

func renderOperations(b *strings.Builder, doc *v3high.Document) {
	if doc.Paths == nil || doc.Paths.PathItems == nil || doc.Paths.PathItems.Len() == 0 {
		return
	}

	// Collect (method, path, op, pathParams) tuples preserving spec
	// order. Path-level parameters are captured here so each operation
	// renders with the full merged parameter set.
	var slots []opSlot
	for pair := doc.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		path := pair.Key()
		item := pair.Value()
		for opPair := item.GetOperations().First(); opPair != nil; opPair = opPair.Next() {
			slots = append(slots, opSlot{
				method:     strings.ToUpper(opPair.Key()),
				path:       path,
				op:         opPair.Value(),
				pathParams: item.Parameters,
			})
		}
	}

	// Group by primary tag (first declared) — "Other" for the untagged.
	groups := map[string][]opSlot{}
	var tagOrder []string
	seen := map[string]bool{}
	for _, s := range slots {
		tag := "Other"
		if len(s.op.Tags) > 0 && strings.TrimSpace(s.op.Tags[0]) != "" {
			tag = s.op.Tags[0]
		}
		if !seen[tag] {
			tagOrder = append(tagOrder, tag)
			seen[tag] = true
		}
		groups[tag] = append(groups[tag], s)
	}

	b.WriteString("## Endpoints\n\n")
	for _, tag := range tagOrder {
		fmt.Fprintf(b, "### %s\n\n", tag)
		for _, slot := range groups[tag] {
			renderOperation(b, slot)
		}
	}
}

func renderOperation(b *strings.Builder, slot opSlot) {
	fmt.Fprintf(b, "#### `%s %s`\n\n", slot.method, slot.path)
	if s := strings.TrimSpace(slot.op.Summary); s != "" {
		b.WriteString("**Summary:** ")
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if d := strings.TrimSpace(slot.op.Description); d != "" {
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	if slot.op.Deprecated != nil && *slot.op.Deprecated {
		b.WriteString("> ⚠ **Deprecated.**\n\n")
	}
	if slot.op.OperationId != "" {
		fmt.Fprintf(b, "**Operation ID:** `%s`\n\n", slot.op.OperationId)
	}
	if params := mergeParameters(slot.pathParams, slot.op.Parameters); len(params) > 0 {
		renderParameters(b, params)
	}
	if slot.op.RequestBody != nil {
		renderRequestBody(b, slot.op.RequestBody)
	}
	if slot.op.Responses != nil {
		renderResponses(b, slot.op.Responses)
	}
}

// mergeParameters combines path-item-level parameters with
// operation-level ones, deduping on the (name, in) tuple. Operation-
// level parameters win on conflict — that's the OpenAPI spec's rule.
// Order is: path-level first (in declared order), then operation-
// level entries that don't collide, then operation-level overrides
// inserted in place of the path-level entry they override.
func mergeParameters(pathParams, opParams []*v3high.Parameter) []*v3high.Parameter {
	if len(pathParams) == 0 {
		return opParams
	}
	if len(opParams) == 0 {
		return pathParams
	}
	type key struct{ name, in string }
	override := map[key]*v3high.Parameter{}
	for _, p := range opParams {
		if p == nil {
			continue
		}
		override[key{p.Name, p.In}] = p
	}
	out := make([]*v3high.Parameter, 0, len(pathParams)+len(opParams))
	seen := map[key]bool{}
	for _, p := range pathParams {
		if p == nil {
			continue
		}
		k := key{p.Name, p.In}
		if rep, ok := override[k]; ok {
			out = append(out, rep)
		} else {
			out = append(out, p)
		}
		seen[k] = true
	}
	for _, p := range opParams {
		if p == nil {
			continue
		}
		k := key{p.Name, p.In}
		if seen[k] {
			continue
		}
		out = append(out, p)
		seen[k] = true
	}
	return out
}

func renderParameters(b *strings.Builder, params []*v3high.Parameter) {
	if len(params) == 0 {
		return
	}
	b.WriteString("**Parameters**\n\n")
	b.WriteString("| Name | In | Required | Type | Description |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, p := range params {
		required := "no"
		if p.Required != nil && *p.Required {
			required = "yes"
		}
		typ := schemaTypeBrief(p.Schema)
		desc := singleLine(p.Description)
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n",
			escapeTableCell(p.Name),
			escapeTableCell(p.In),
			required,
			codeOrBlank(typ),
			desc,
		)
	}
	b.WriteString("\n")
}

func renderRequestBody(b *strings.Builder, rb *v3high.RequestBody) {
	b.WriteString("**Request body**")
	if rb.Required != nil && *rb.Required {
		b.WriteString(" *(required)*")
	}
	b.WriteString("\n\n")
	if d := strings.TrimSpace(rb.Description); d != "" {
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	if rb.Content != nil {
		for pair := rb.Content.First(); pair != nil; pair = pair.Next() {
			fmt.Fprintf(b, "- Content-Type: `%s`", pair.Key())
			if t := schemaTypeBrief(pair.Value().Schema); t != "" {
				fmt.Fprintf(b, " — %s", t)
			}
			b.WriteString("\n")
			renderMediaExample(b, pair.Value())
		}
		b.WriteString("\n")
	}
}

func renderResponses(b *strings.Builder, resps *v3high.Responses) {
	if resps == nil {
		return
	}
	hasAny := (resps.Codes != nil && resps.Codes.Len() > 0) || resps.Default != nil
	if !hasAny {
		return
	}
	b.WriteString("**Responses**\n\n")
	b.WriteString("| Code | Description |\n")
	b.WriteString("| --- | --- |\n")
	if resps.Codes != nil {
		for pair := resps.Codes.First(); pair != nil; pair = pair.Next() {
			desc := ""
			if r := pair.Value(); r != nil {
				desc = singleLine(r.Description)
			}
			fmt.Fprintf(b, "| `%s` | %s |\n", escapeTableCell(pair.Key()), desc)
		}
	}
	if resps.Default != nil {
		fmt.Fprintf(b, "| `default` | %s |\n", singleLine(resps.Default.Description))
	}
	b.WriteString("\n")
}

func renderMediaExample(b *strings.Builder, mt *v3high.MediaType) {
	if mt == nil || mt.Example == nil {
		return
	}
	// Scalar nodes carry their literal text in .Value; mapping and
	// sequence nodes don't, so we serialize the whole node tree to
	// YAML to get a usable representation. Tag/anchor metadata is
	// untouched — we're rendering whatever the spec author wrote.
	example := strings.TrimSpace(mt.Example.Value)
	if example == "" {
		out, err := yaml.Marshal(mt.Example)
		if err != nil {
			return
		}
		example = strings.TrimSpace(string(out))
	}
	if example == "" {
		return
	}
	b.WriteString("\n  Example:\n\n  ```\n  ")
	b.WriteString(strings.ReplaceAll(example, "\n", "\n  "))
	b.WriteString("\n  ```\n")
}

func renderSchemas(b *strings.Builder, doc *v3high.Document) {
	if doc.Components == nil || doc.Components.Schemas == nil || doc.Components.Schemas.Len() == 0 {
		return
	}
	b.WriteString("## Schemas\n\n")

	// Sort schema names for stable output.
	var names []string
	for pair := doc.Components.Schemas.First(); pair != nil; pair = pair.Next() {
		names = append(names, pair.Key())
	}
	sort.Strings(names)

	for _, name := range names {
		proxy, _ := doc.Components.Schemas.Get(name)
		if proxy == nil {
			continue
		}
		fmt.Fprintf(b, "### `%s`\n\n", name)
		schema := proxy.Schema()
		if schema == nil {
			b.WriteString("*(unresolved schema)*\n\n")
			continue
		}
		if d := strings.TrimSpace(schema.Description); d != "" {
			b.WriteString(d)
			b.WriteString("\n\n")
		}
		renderSchemaProperties(b, schema)
	}
}

func renderSchemaProperties(b *strings.Builder, s *base.Schema) {
	if s == nil || s.Properties == nil || s.Properties.Len() == 0 {
		return
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	b.WriteString("| Property | Type | Required | Description |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for pair := s.Properties.First(); pair != nil; pair = pair.Next() {
		name := pair.Key()
		typ := schemaTypeBrief(pair.Value())
		desc := ""
		if sub := pair.Value().Schema(); sub != nil {
			desc = singleLine(sub.Description)
		}
		req := "no"
		if required[name] {
			req = "yes"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n",
			escapeTableCell(name),
			codeOrBlank(typ),
			req,
			desc,
		)
	}
	b.WriteString("\n")
}

// codeOrBlank wraps t in backticks unless t is empty. schemaTypeBrief
// already wraps `$ref` values in backticks so we strip any existing
// pair to avoid “ “Pet“ “ double-wrap.
func codeOrBlank(t string) string {
	if t == "" {
		return ""
	}
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "`") && strings.HasSuffix(t, "`") {
		return t
	}
	return "`" + escapeTableCell(t) + "`"
}

// schemaTypeBrief returns a one-line type description for a schema
// proxy: the declared type, format if present, item type for arrays,
// or "ref" for unresolved references.
func schemaTypeBrief(p *base.SchemaProxy) string {
	if p == nil {
		return ""
	}
	if p.IsReference() {
		ref := p.GetReference()
		// Trim the canonical `#/components/schemas/` prefix to keep
		// the type cell readable.
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			return fmt.Sprintf("`%s`", ref[i+1:])
		}
		return fmt.Sprintf("`%s`", ref)
	}
	s := p.Schema()
	if s == nil {
		return ""
	}
	var parts []string
	if len(s.Type) > 0 {
		parts = append(parts, s.Type[0])
	}
	if s.Format != "" {
		parts = append(parts, fmt.Sprintf("(%s)", s.Format))
	}
	if len(s.Type) > 0 && s.Type[0] == "array" && s.Items != nil && s.Items.A != nil {
		sub := schemaTypeBrief(s.Items.A)
		if sub != "" {
			parts = append(parts, "of "+sub)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// singleLine collapses internal whitespace and newlines for safe
// inclusion in a markdown table cell.
func singleLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r", "")
	// Replace newlines with spaces, collapse runs of whitespace.
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return escapeTableCell(s)
}

// escapeTableCell escapes characters that would break a markdown table
// row: `|` becomes `\|`, and existing backslashes are doubled so the
// escape stays valid.
func escapeTableCell(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
}
