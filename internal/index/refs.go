package index

import (
	"bytes"
	"fmt"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// CallRef is one function-call site: the calling file/line and the callee
// name. Stored in index_calls for the code_references tool.
type CallRef struct {
	Path   string
	Line   int
	Callee string
}

// callQueries extract callee names per language. Multiple patterns are
// separated by newlines. Queries that fail to compile for a grammar are
// skipped gracefully (call refs then just aren't indexed for that file).
var callQueries = map[string]string{
	"go": `
(call_expression function: (identifier) @callee)
(call_expression function: (selector_expression field: (field_identifier) @callee))
`,
	"python": `
(call function: (identifier) @callee)
(call function: (attribute attribute: (identifier) @callee))
`,
	"javascript": `
(call_expression function: (identifier) @callee)
(call_expression function: (member_expression property: (property_identifier) @callee))
`,
	"typescript": `
(call_expression function: (identifier) @callee)
(call_expression function: (member_expression property: (property_identifier) @callee))
`,
	"rust": `
(call_expression function: (identifier) @callee)
(call_expression function: (field_expression field: (field_identifier) @callee))
`,
	"c": `
(call_expression function: (identifier) @callee)
`,
	"cpp": `
(call_expression function: (identifier) @callee)
(call_expression function: (field_expression field: (field_identifier) @callee))
`,
	"java": `
(method_invocation name: (identifier) @callee)
`,
}

// refsFromTree collects CallRefs from an already-parsed root node.
func refsFromTree(path string, lang *language, root *tree_sitter.Node, content []byte) []CallRef {
	src, ok := callQueries[lang.name]
	if !ok || src == "" {
		return nil
	}
	q, qerr := tree_sitter.NewQuery(lang.lang, src)
	if qerr != nil {
		return nil
	}
	defer q.Close()

	seen := map[string]bool{}
	var refs []CallRef
	cursor := tree_sitter.NewQueryCursor()
	defer cursor.Close()
	matches := cursor.Matches(q, root, content)
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		for _, cap := range m.Captures {
			if cap.Node.Kind() != "identifier" && cap.Node.Kind() != "field_identifier" &&
				cap.Node.Kind() != "property_identifier" {
				continue
			}
			callee := cap.Node.Utf8Text(content)
			if callee == "" || isKeyword(callee) {
				continue
			}
			line := int(cap.Node.StartPosition().Row) + 1
			key := fmt.Sprintf("%s:%d:%d", callee, cap.Node.StartByte(), cap.Node.EndByte()) // dedupe same-site double captures
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, CallRef{Path: path, Line: line, Callee: callee})
		}
	}
	return refs
}

// isKeyword filters control-flow names that appear as call-like nodes in some
// grammars (go, defer, return-adjacent identifiers are not real calls).
func isKeyword(s string) bool {
	switch s {
	case "go", "defer", "if", "for", "return", "new", "make", "len", "cap", "append", "print", "println":
		return true
	}
	return false
}

// calleeRefBytes renders refs for a test/debug reader.
func calleeRefBytes(refs []CallRef) []byte {
	var b bytes.Buffer
	for _, r := range refs {
		b.WriteString(r.Callee)
		b.WriteByte(' ')
	}
	return b.Bytes()
}
