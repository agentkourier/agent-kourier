package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// apiPackage is what the generator knows of one Go package of API types.
type apiPackage struct {
	structs map[string]*ast.StructType
	docs    map[string]*ast.CommentGroup
	order   []string            // type names in source order
	consts  map[string][]string // a named type's constant values, in source order
}

// apiReference returns one Markdown page for each resource kind of the Go package in dir (a type marked
// +kubebuilder:object:root=true that is not a List), keyed by the kind. from names the package in the page's text.
func apiReference(dir, from, apiVersion string) (map[string]string, error) {
	pkg, err := readPackage(dir)
	if err != nil {
		return nil, err
	}
	pages := map[string]string{}
	for _, name := range pkg.order {
		doc := pkg.docs[name]
		if strings.HasSuffix(name, "List") || !slices.Contains(markers(doc), "+kubebuilder:object:root=true") {
			continue
		}
		pages[name] = pkg.page(name, apiVersion, from)
	}
	return pages, nil
}

func readPackage(dir string) (*apiPackage, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	slices.Sort(paths)
	pkg := &apiPackage{structs: map[string]*ast.StructType{}, docs: map[string]*ast.CommentGroup{}, consts: map[string][]string{}}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // the path is the package's own file
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gen.Tok {
			case token.TYPE:
				pkg.addTypes(gen)
			case token.CONST:
				pkg.addConsts(gen)
			}
		}
	}
	return pkg, nil
}

func (p *apiPackage) addTypes(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		ts := spec.(*ast.TypeSpec) //nolint:forcetypeassert // a type declaration holds type specs
		doc := ts.Doc
		if doc == nil && len(gen.Specs) == 1 {
			doc = gen.Doc
		}
		p.docs[ts.Name.Name] = doc
		p.order = append(p.order, ts.Name.Name)
		if st, ok := ts.Type.(*ast.StructType); ok {
			p.structs[ts.Name.Name] = st
		}
	}
}

func (p *apiPackage) addConsts(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		vs := spec.(*ast.ValueSpec) //nolint:forcetypeassert // a const declaration holds value specs
		typ, ok := vs.Type.(*ast.Ident)
		if !ok {
			continue
		}
		for _, v := range vs.Values {
			if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					p.consts[typ.Name] = append(p.consts[typ.Name], s)
				}
			}
		}
	}
}

// page writes the page of the root kind: the kind's own fields first, then each struct it reaches, depth first in
// field order, each once.
func (p *apiPackage) page(kind, apiVersion, from string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", kind)
	fmt.Fprintf(&b, "This page lists every field of the %s resource, as the Go types in `%s` define it.\n\n", kind, from)
	fmt.Fprintf(&b, "`apiVersion: %s` · `kind: %s`\n\n", apiVersion, kind)
	if d := description(p.docs[kind]); d != "" {
		b.WriteString(escapeText(d, false) + "\n")
	}
	seen := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		st := p.structs[name]
		b.WriteString("\n## " + name + "\n\n")
		if d := description(p.docs[name]); d != "" && name != kind {
			b.WriteString(escapeText(d, false) + "\n\n")
		}
		b.WriteString("| Field | Type | Required | Default | Description |\n|---|---|---|---|---|\n")
		var next []string
		for _, f := range st.Fields.List {
			if row, ref, ok := p.row(name == kind, f); ok {
				b.WriteString(row)
				if ref != "" {
					next = append(next, ref)
				}
			}
		}
		for _, n := range next {
			visit(n)
		}
	}
	visit(kind)
	b.WriteString(generatedNote(from))
	return b.String()
}

// row returns the table row of one field, and the name of the package's struct the field holds, if any.
func (p *apiPackage) row(rootKind bool, f *ast.Field) (string, string, bool) {
	tag := ""
	if f.Tag != nil {
		if s, err := strconv.Unquote(f.Tag.Value); err == nil {
			tag = reflect.StructTag(s).Get("json")
		}
	}
	name, opts, _ := strings.Cut(tag, ",")
	if len(f.Names) == 0 { // embedded: TypeMeta is apiVersion and kind; ObjectMeta is metadata
		if sel, ok := f.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ObjectMeta" && rootKind {
			return "| `metadata` | ObjectMeta | Yes |  | Standard Kubernetes object metadata. Agent Kourier reads `name` and `namespace`. |\n", "", true
		}
		return "", "", false
	}
	if !f.Names[0].IsExported() || name == "" || name == "-" {
		return "", "", false
	}
	ms := markers(f.Doc)
	optional := strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero") || slices.Contains(ms, "+optional")
	if _, ptr := f.Type.(*ast.StarExpr); ptr {
		optional = true
	}
	required := "Yes"
	if optional {
		required = "No"
	}
	typ, ref := p.typeName(f.Type)
	def := ""
	if v, ok := marker(ms, "+kubebuilder:default="); ok {
		def = "`" + strings.Trim(v, `"`) + "`"
	}
	desc := escapeText(description(f.Doc), true)
	var extra []string
	if v, ok := marker(ms, "+kubebuilder:validation:Enum="); ok {
		extra = append(extra, "One of "+codeList(strings.Split(v, ";"))+".")
	}
	if keys := p.mapKeys(f.Type); len(keys) > 0 {
		extra = append(extra, "Keys: "+codeList(keys)+".")
	}
	if v, ok := marker(ms, "+kubebuilder:validation:Pattern="); ok {
		extra = append(extra, "Pattern `"+strings.Trim(v, "`")+"`.")
	}
	if v, ok := marker(ms, "+kubebuilder:validation:Minimum="); ok {
		extra = append(extra, "Minimum "+v+".")
	}
	if v, ok := marker(ms, "+kubebuilder:validation:MinLength="); ok {
		extra = append(extra, "At least "+v+" character"+plural(v)+".")
	}
	if v, ok := marker(ms, "+kubebuilder:validation:MaxLength="); ok {
		extra = append(extra, "At most "+v+" character"+plural(v)+".")
	}
	if len(extra) > 0 {
		desc = strings.TrimSpace(desc + " " + strings.Join(extra, " "))
	}
	return fmt.Sprintf("| `%s` | %s | %s | %s | %s |\n", name, typ, required, def, desc), ref, true
}

func plural(n string) string {
	if n == "1" {
		return ""
	}
	return "s"
}

func codeList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "`" + v + "`"
	}
	return strings.Join(quoted, ", ")
}

// typeName is how a field's type reads in the table, and the package's struct it holds, if any.
func (p *apiPackage) typeName(expr ast.Expr) (string, string) {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return p.typeName(t.X)
	case *ast.ArrayType:
		s, ref := p.typeName(t.Elt)
		return "[]" + s, ref
	case *ast.MapType:
		k, _ := p.typeName(t.Key)
		v, ref := p.typeName(t.Value)
		return "map[" + k + "]" + v, ref
	case *ast.SelectorExpr:
		if t.Sel.Name == "Duration" {
			return "duration", ""
		}
		return t.Sel.Name, ""
	case *ast.Ident:
		switch t.Name {
		case "string":
			return "string", ""
		case "bool":
			return "boolean", ""
		case "int", "int32", "int64":
			return "integer", ""
		}
		if _, ok := p.structs[t.Name]; ok {
			return "[" + t.Name + "](#" + strings.ToLower(t.Name) + ")", t.Name
		}
		return "string", "" // a named string type, such as an enumeration
	}
	return fmt.Sprintf("%T", expr), ""
}

// mapKeys lists the constants of a map's named key type, which are the keys it may hold.
func (p *apiPackage) mapKeys(expr ast.Expr) []string {
	if m, ok := expr.(*ast.MapType); ok {
		if id, ok := m.Key.(*ast.Ident); ok {
			return p.consts[id.Name]
		}
	}
	return nil
}

// markers returns the comment's marker lines, such as +optional.
func markers(doc *ast.CommentGroup) []string {
	if doc == nil {
		return nil
	}
	var ms []string
	for line := range strings.SplitSeq(doc.Text(), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "+") {
			ms = append(ms, line)
		}
	}
	return ms
}

func marker(ms []string, prefix string) (string, bool) {
	for _, m := range ms {
		if v, ok := strings.CutPrefix(m, prefix); ok {
			return v, true
		}
	}
	return "", false
}

// description is the comment's prose without its markers and its maintainer references, its lines joined into one
// paragraph.
func description(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	var words []string
	for line := range strings.SplitSeq(doc.Text(), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "+") {
			words = append(words, line)
		}
	}
	return readerText(strings.Join(words, " "))
}
