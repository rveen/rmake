// Package syntax implements the lexer, parser, AST and printer of the rmake
// build language (README §5).
package syntax

import "fmt"

// Pos is a source position. Line and Col are 1-based; Col counts bytes.
type Pos struct {
	File string
	Line int
	Col  int
}

func (p Pos) String() string {
	if p.File == "" {
		return fmt.Sprintf("%d:%d", p.Line, p.Col)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Error is an error at a source position.
type Error struct {
	Pos Pos
	Msg string
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

// Errorf returns an *Error at pos.
func Errorf(pos Pos, format string, args ...any) error {
	return &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// Part is one piece of a word: literal text or a variable reference.
type Part struct {
	Pos    Pos
	Lit    string // literal text, when Var == ""
	Var    string // variable name
	Subst  *Subst // ${Var:pat=repl}, optional
	Quoted bool   // inside "...": a list value is joined with single spaces
}

// Subst is the substitution of a reference ${x:pat=repl}.
type Subst struct {
	Pat, Repl []Part
}

// Word is a whitespace-separated word. Raw is its exact source text, which
// the printer reproduces.
type Word struct {
	Pos   Pos
	Raw   string
	Parts []Part
}

// Literal returns the word's text if it contains no variable references.
func (w Word) Literal() (string, bool) {
	s := ""
	for _, p := range w.Parts {
		if p.Var != "" {
			return "", false
		}
		s += p.Lit
	}
	return s, true
}

// Node holds what every statement and attribute has in common.
type Node struct {
	Pos         Pos
	Comments    []string // full-line comments directly before, with '#'
	LineComment string   // comment at the end of the line, with '#'
	BlankBefore bool     // a blank line preceded it in the source
}

// N returns the node itself; it lets *Node satisfy Stmt via embedding.
func (n *Node) N() *Node { return n }

// Stmt is a top-level (or if-body) statement.
type Stmt interface{ N() *Node }

// File is a parsed build file.
type File struct {
	Name     string
	Stmts    []Stmt
	Trailing []string // comments after the last statement
}

// Var is `var NAME (=|+=|?=) value`. It is also used for rule/task-local
// variables.
type Var struct {
	Node
	Name   string
	Op     string // "=", "+=" or "?="
	Glob   bool   // value starts with the keyword glob
	Values []Word
}

// Include is `include path`.
type Include struct {
	Node
	Path Word
}

// Default is `default target...`.
type Default struct {
	Node
	Targets []Word
}

// Rule is `rule outs from ins [| implicit]` plus attributes.
type Rule struct {
	Node
	Outs     []Word
	Ins      []Word
	Pipe     bool // a '|' was present
	Implicit []Word
	Attrs    []*Attr
}

// Task is `task NAME[(params)] [needs targets]` plus attributes.
type Task struct {
	Node
	Name   string
	Params []string
	Needs  []Word
	Attrs  []*Attr
}

// If is `if cond ... [else ...] end`.
type If struct {
	Node
	Defined string // for `if defined NAME`
	Left    Word
	Op      string // "==", "!=" or "defined"
	Right   Word
	Then    []Stmt
	Else    []Stmt
	HasElse bool
	ElseN   Node // comments before else / on its line
	EndN    Node // comments before end / on its line
}

// Attr is an indented attribute line of a rule or task.
type Attr struct {
	Node
	Kind string // run, sh, deps, depfile, pass, always, desc, var
	Args []Word
	Var  *Var // when Kind == "var"
}
