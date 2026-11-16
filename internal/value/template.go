package value

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
)

func parseTemplate(source string) (*template.Template, error) {
	t, err := template.New("file").Funcs(TemplateFunctions()).Option("missingkey=error").Parse(source)
	if err != nil {
		return nil, explainWithheld(err)
	}
	return t, nil
}

// TemplateReads lists the root references a file template reads, sorted; "."
// is the whole root. Validation, secret names, and delivery use this list.
func TemplateReads(source string) ([]string, error) {
	t, err := parseTemplate(source)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	if err := walkTemplate(t, func(path string) parse.Node { found[path] = true; return nil }); err != nil {
		return nil, err
	}
	reads := make([]string, 0, len(found))
	for path := range found {
		reads = append(reads, path)
	}
	sort.Strings(reads)
	return reads, nil
}

// templateWalk is the one parse-tree walker. It calls read for every read of the
// root data: a field, a $ path, a dot, a parenthesized read followed by
// fields, or an index of a read whose keys are literals or reads. That index
// reads the selector path it names, such as .outputs.inspect[.node.name], so
// only the selected value is checked, traced, and delivered; an index with any
// other key reads its whole collection. A non-nil result replaces that
// argument, or the whole index command. Dot and $ are tracked as the
// root path they hold, or "" when they hold a value not reachable by a path
// (a range element, a computed value). A template call reads its argument; the
// called body sees only that value. Any other node kind is an error, so
// template syntax cannot hide a read from validation, secret tracing, or
// secret delivery.
type templateWalk struct {
	read func(string) parse.Node
}

func walkTemplate(t *template.Template, read func(string) parse.Node) error {
	if t.Tree == nil {
		return nil
	}
	w := &templateWalk{read: read}
	return w.walk(t.Tree.Root, ".", ".")
}
func (w *templateWalk) walk(node parse.Node, dot, dollar string) error {
	switch n := node.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				if err := w.walk(child, dot, dollar); err != nil {
					return err
				}
			}
		}
	case *parse.PipeNode:
		if n != nil {
			for i, cmd := range n.Cmds {
				// A later command also receives the previous result as its final key.
				if path := indexPath(cmd.Args, dot, dollar); i == 0 && path != "" {
					if replacement := w.read(path); replacement != nil {
						cmd.Args = []parse.Node{replacement}
					}
				} else if err := w.walk(cmd, dot, dollar); err != nil {
					return err
				}
			}
		}
	case *parse.CommandNode:
		for i, arg := range n.Args {
			if path := rootPath(arg, dot, dollar); path != "" {
				if replacement := w.read(path); replacement != nil {
					n.Args[i] = replacement
				}
			} else if err := w.walk(arg, dot, dollar); err != nil {
				return err
			}
		}
	case *parse.ChainNode:
		return w.walk(n.Node, dot, dollar)
	case *parse.ActionNode:
		return w.walk(n.Pipe, dot, dollar)
	case *parse.TemplateNode:
		return w.walk(n.Pipe, dot, dollar)
	case *parse.IfNode:
		return w.branch(&n.BranchNode, dot, dollar, dot)
	case *parse.RangeNode:
		return w.branch(&n.BranchNode, dot, dollar, "")
	case *parse.WithNode:
		return w.branch(&n.BranchNode, dot, dollar, "")
	case *parse.DotNode, *parse.FieldNode, *parse.VariableNode:
		// Relative to a value already read at its source.
	case nil, *parse.TextNode, *parse.CommentNode, *parse.IdentifierNode, *parse.StringNode, *parse.NumberNode, *parse.BoolNode, *parse.NilNode, *parse.BreakNode, *parse.ContinueNode:
	default:
		return fmt.Errorf("template syntax %q is not supported", node.String())
	}
	return nil
}
func (w *templateWalk) branch(n *parse.BranchNode, dot, dollar, bodyDot string) error {
	if err := w.walk(n.Pipe, dot, dollar); err != nil {
		return err
	}
	if err := w.walk(n.List, bodyDot, dollar); err != nil {
		return err
	}
	return w.walk(n.ElseList, dot, dollar)
}

// rootPath is the root reference an argument reads, or "".
func rootPath(node parse.Node, dot, dollar string) string {
	switch n := node.(type) {
	case *parse.DotNode:
		return dot
	case *parse.FieldNode:
		return extendPath(dot, n.Ident)
	case *parse.VariableNode:
		if n.Ident[0] == "$" {
			return extendPath(dollar, n.Ident[1:])
		}
	case *parse.PipeNode:
		return pipePath(n, dot, dollar)
	case *parse.ChainNode:
		if pipe, ok := n.Node.(*parse.PipeNode); ok {
			return extendPath(pipePath(pipe, dot, dollar), n.Field)
		}
	}
	return ""
}

// pipePath is the root reference a parenthesized pipeline reads: one read, or
// an index of one, or "".
func pipePath(pipe *parse.PipeNode, dot, dollar string) string {
	if len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 {
		return ""
	}
	if args := pipe.Cmds[0].Args; len(args) == 1 {
		return rootPath(args[0], dot, dollar)
	}
	return indexPath(pipe.Cmds[0].Args, dot, dollar)
}

// indexPath is the selector path an index command reads, or "" when it is not
// index or a key is not a string, nonnegative integer, or root read.
func indexPath(args []parse.Node, dot, dollar string) string {
	if len(args) < 2 {
		return ""
	}
	if name, ok := args[0].(*parse.IdentifierNode); !ok || name.Ident != "index" {
		return ""
	}
	path := rootPath(args[1], dot, dollar)
	if path == "." {
		return ""
	}
	for _, arg := range args[2:] {
		key := ""
		switch k := arg.(type) {
		case *parse.StringNode:
			key = strconv.Quote(k.Text)
		case *parse.NumberNode:
			if k.IsInt && k.Int64 >= 0 {
				key = strconv.FormatInt(k.Int64, 10)
			}
		default:
			if key = rootPath(arg, dot, dollar); key == "." {
				key = ""
			}
		}
		if path == "" || key == "" {
			return ""
		}
		path += "[" + key + "]"
	}
	return path
}
func extendPath(base string, fields []string) string {
	if base == "" || len(fields) == 0 {
		return base
	}
	return strings.TrimSuffix(base, ".") + "." + strings.Join(fields, ".")
}

// Root reads go through the same availability checks as YAML references.
// Checking at evaluation time preserves Go template if/and/or short-circuiting.
func guardTemplate(t *template.Template) error {
	return walkTemplate(t, func(path string) parse.Node {
		return &parse.PipeNode{NodeType: parse.NodePipe, Cmds: []*parse.CommandNode{{NodeType: parse.NodeCommand, Args: []parse.Node{
			parse.NewIdentifier("mmx_value"), &parse.StringNode{NodeType: parse.NodeString, Quoted: fmt.Sprintf("%q", path), Text: path},
		}}}}
	})
}
