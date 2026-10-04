package v1alpha1

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
)

// PromptRefs is what a prompt template reads from its PromptView, found without running the
// template, so it covers every branch.
type PromptRefs struct {
	// View lists the PromptView fields the template reads other than Fields (Channel, State,
	// Key, Link, Source), sorted and without repeats.
	View []string
	// Fields lists the names of the .Fields entries it reads, sorted and without repeats.
	Fields []string
}

// TemplateRefs walks the parse trees of tmpl, the template itself and every template it
// defines, and returns what they read from a PromptView. A template that reads the view any
// other way is refused, because the renderer (internal/render) checks the characters of exactly
// the values a template names, and a value read in another way would escape that check. The
// accepted forms are:
//
//   - .Channel, .State, .Key, .Link and .Source, or the same behind $ (as in $.State);
//   - .Fields.name, or $.Fields.name, or index .Fields "name" with a constant string.
//
// Refused are a field that PromptView lacks, .Fields on its own (it could be ranged over or
// printed whole), index .Fields with anything but one constant name, a field of a value that is
// only text (.State.Name), and . or $ read as a value, which would be the whole view. The name
// of an entry is not checked against the trigger's extract here, as that is the trigger's: see
// checkPromptTemplate. An error names the position as name:line:column.
func TemplateRefs(tmpl *template.Template) (PromptRefs, error) {
	w := newRefWalker(nil, false)
	if err := w.walkAll(tmpl); err != nil {
		return PromptRefs{}, err
	}
	return w.refs(), nil
}

// refWalker walks parse trees and collects what they read of a PromptView. The first problem
// it finds stops it.
type refWalker struct {
	tree    *parse.Tree
	view    map[string]bool // the PromptView fields
	extract []string        // sorted names the entries must be among, if checkExtract
	checkEx bool
	seen    map[string]bool // PromptView fields read other than Fields
	fields  map[string]bool // names read under .Fields
	err     error
}

// newRefWalker returns a walker that, if check is set, also requires every .Fields entry to be
// one of extract (none, if it is empty).
func newRefWalker(extract []string, check bool) *refWalker {
	w := &refWalker{view: map[string]bool{}, seen: map[string]bool{}, fields: map[string]bool{}}
	rt := reflect.TypeFor[PromptView]()
	for f := range rt.Fields() {
		w.view[f.Name] = true
	}
	w.checkEx = check
	w.extract = slices.Sorted(slices.Values(extract))
	return w
}

func (w *refWalker) refs() PromptRefs {
	return PromptRefs{View: slices.Sorted(maps.Keys(w.seen)), Fields: slices.Sorted(maps.Keys(w.fields))}
}

// walkAll walks tmpl and the templates defined with it, in name order so that the error is the
// same on every run.
func (w *refWalker) walkAll(tmpl *template.Template) error {
	all := tmpl.Templates()
	slices.SortFunc(all, func(a, b *template.Template) int { return strings.Compare(a.Name(), b.Name()) })
	for _, t := range all {
		if t.Tree == nil || t.Root == nil {
			continue // a template named by {{ template }} and never defined
		}
		w.tree = t.Tree
		w.node(t.Root)
		if w.err != nil {
			return w.err
		}
	}
	return nil
}

// fail records the first problem, located at n.
func (w *refWalker) fail(n parse.Node, format string, args ...any) {
	if w.err != nil {
		return
	}
	loc, _ := w.tree.ErrorContext(n)
	w.err = fmt.Errorf("%s: %s", loc, fmt.Sprintf(format, args...))
}

func (w *refWalker) node(n parse.Node) {
	if w.err != nil {
		return
	}
	switch n := n.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, c := range n.Nodes {
				w.node(c)
			}
		}
	case *parse.ActionNode:
		w.node(n.Pipe)
	case *parse.IfNode:
		w.branch(&n.BranchNode)
	case *parse.RangeNode:
		w.branch(&n.BranchNode)
	case *parse.WithNode:
		w.branch(&n.BranchNode)
	case *parse.TemplateNode:
		if n.Pipe != nil {
			w.node(n.Pipe)
		}
	case *parse.PipeNode:
		if n != nil {
			for _, c := range n.Cmds {
				w.command(c)
			}
		}
	}
}

func (w *refWalker) branch(b *parse.BranchNode) {
	w.node(b.Pipe)
	w.node(b.List)
	w.node(b.ElseList)
}

// command reads one command of a pipeline: a function call or a value.
func (w *refWalker) command(c *parse.CommandNode) {
	if len(c.Args) > 0 && isIdentifier(c.Args[0], "index") && len(c.Args) > 1 && w.isFields(c.Args[1]) {
		// index .Fields "name" is a reference; it is the only form of index that may read .Fields.
		if len(c.Args) != 3 {
			w.fail(c, "index .Fields takes one constant name, as in index .Fields \"alertname\"")
			return
		}
		name, ok := c.Args[2].(*parse.StringNode)
		if !ok {
			w.fail(c, "index .Fields takes one constant name, as in index .Fields \"alertname\"")
			return
		}
		w.entry(c, name.Text)
		return
	}
	for _, a := range c.Args {
		w.operand(a)
	}
}

func isIdentifier(n parse.Node, name string) bool {
	id, ok := n.(*parse.IdentifierNode)
	return ok && id.Ident == name
}

// isFields reports whether n is .Fields or $.Fields and nothing more.
func (w *refWalker) isFields(n parse.Node) bool {
	switch n := n.(type) {
	case *parse.FieldNode:
		return len(n.Ident) == 1 && n.Ident[0] == "Fields"
	case *parse.VariableNode:
		return len(n.Ident) == 2 && n.Ident[0] == "$" && n.Ident[1] == "Fields"
	}
	return false
}

// operand reads one argument of a command.
func (w *refWalker) operand(n parse.Node) {
	switch n := n.(type) {
	case *parse.FieldNode:
		w.field(n, n.Ident)
	case *parse.VariableNode:
		if n.Ident[0] != "$" {
			return // a variable of the template: what it holds was read where it was set
		}
		if len(n.Ident) == 1 {
			w.fail(n, "$ is the whole prompt view: name the field you want, such as .State")
			return
		}
		w.field(n, n.Ident[1:])
	case *parse.DotNode:
		w.fail(n, ". may hold the whole prompt view: name the field you want, such as .State")
	case *parse.ChainNode:
		w.operand(n.Node)
	case *parse.PipeNode:
		w.node(n)
	}
}

// field reads a field chain from the view: its first name must be a PromptView field.
func (w *refWalker) field(n parse.Node, ident []string) {
	name := ident[0]
	switch {
	case !w.view[name]:
		w.fail(n, "no field .%s in the prompt view (it has %s)", name, w.viewList())
	case name != "Fields":
		if len(ident) > 1 {
			w.fail(n, ".%s holds text, which has no field .%s", name, ident[1])
			return
		}
		w.seen[name] = true
	case len(ident) == 1:
		w.fail(n, ".Fields as a whole cannot be read: name an entry, such as .Fields.alertname")
	case len(ident) > 2:
		w.fail(n, ".Fields.%s holds text, which has no field .%s", ident[1], ident[2])
	default:
		w.entry(n, ident[1])
	}
}

// entry records a read of .Fields.name, and checks name against the extract if there is one.
func (w *refWalker) entry(n parse.Node, name string) {
	if w.checkEx && !slices.Contains(w.extract, name) {
		have := "extract has none"
		if len(w.extract) > 0 {
			have = "extract has " + strings.Join(w.extract, ", ")
		}
		w.fail(n, ".Fields.%s is not an extract of this trigger (%s)", name, have)
		return
	}
	w.fields[name] = true
}

// viewList names the PromptView fields as a template writes them, in the type's order, with
// the last one after "and".
func (w *refWalker) viewList() string {
	rt := reflect.TypeFor[PromptView]()
	names := make([]string, 0, rt.NumField())
	for f := range rt.Fields() {
		names = append(names, "."+f.Name)
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
