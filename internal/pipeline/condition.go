package pipeline

import (
	"fmt"
	"regexp"
	"strings"
)

// Condition is a parsed `if` expression.
//
// The language is deliberately small — comparisons, regex matches, and boolean
// combinators over environment variables — because a CI condition that needs more
// than this is better expressed as a command that exits non-zero. Keeping it small
// also keeps it fully testable and free of evaluation surprises.
//
//	if: $BRANCH == "main"
//	if: $CI != "" && $ENV != "production"
//	if: !($SKIP == "1")
//	if: $TAG =~ "^v[0-9]+"
type Condition struct {
	root node
	src  string
}

// String returns the original expression text.
func (c *Condition) String() string { return c.src }

// Eval evaluates the condition against an environment.
func (c *Condition) Eval(env map[string]string) (bool, error) {
	if c == nil || c.root == nil {
		return true, nil
	}
	return c.root.eval(env)
}

// EvalCondition parses and evaluates in one step. An empty expression is true, so
// a job without an `if` always runs.
func EvalCondition(expr string, env map[string]string) (bool, error) {
	if strings.TrimSpace(expr) == "" {
		return true, nil
	}
	cond, err := ParseCondition(expr)
	if err != nil {
		return false, err
	}
	return cond.Eval(env)
}

// ParseCondition compiles an `if` expression.
func ParseCondition(expr string) (*Condition, error) {
	if strings.TrimSpace(expr) == "" {
		return &Condition{src: expr}, nil
	}
	tokens, err := tokenize(expr)
	if err != nil {
		return nil, err
	}
	p := &condParser{tokens: tokens}
	root, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("unexpected %s at position %d", p.peek().describe(), p.peek().pos)
	}
	return &Condition{root: root, src: expr}, nil
}

// --- tokens ---

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokVar
	tokString
	tokEq
	tokNe
	tokMatch
	tokNotMatch
	tokAnd
	tokOr
	tokNot
	tokLParen
	tokRParen
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

func (t token) describe() string {
	switch t.kind {
	case tokEOF:
		return "end of expression"
	case tokVar:
		return fmt.Sprintf("variable $%s", t.text)
	case tokString:
		return fmt.Sprintf("value %q", t.text)
	default:
		return fmt.Sprintf("token %q", t.text)
	}
}

func tokenize(expr string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(expr) {
		ch := expr[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			i++
		case ch == '(':
			tokens = append(tokens, token{kind: tokLParen, text: "(", pos: i})
			i++
		case ch == ')':
			tokens = append(tokens, token{kind: tokRParen, text: ")", pos: i})
			i++
		case strings.HasPrefix(expr[i:], "&&"):
			tokens = append(tokens, token{kind: tokAnd, text: "&&", pos: i})
			i += 2
		case strings.HasPrefix(expr[i:], "||"):
			tokens = append(tokens, token{kind: tokOr, text: "||", pos: i})
			i += 2
		case strings.HasPrefix(expr[i:], "=="):
			tokens = append(tokens, token{kind: tokEq, text: "==", pos: i})
			i += 2
		case strings.HasPrefix(expr[i:], "!="):
			tokens = append(tokens, token{kind: tokNe, text: "!=", pos: i})
			i += 2
		case strings.HasPrefix(expr[i:], "=~"):
			tokens = append(tokens, token{kind: tokMatch, text: "=~", pos: i})
			i += 2
		case strings.HasPrefix(expr[i:], "!~"):
			tokens = append(tokens, token{kind: tokNotMatch, text: "!~", pos: i})
			i += 2
		case ch == '!':
			tokens = append(tokens, token{kind: tokNot, text: "!", pos: i})
			i++
		case ch == '"' || ch == '\'':
			text, next, err := scanQuoted(expr, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokString, text: text, pos: i})
			i = next
		case ch == '$':
			name, next, err := scanVar(expr, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokVar, text: name, pos: i})
			i = next
		default:
			text, next := scanBare(expr, i)
			if text == "" {
				return nil, fmt.Errorf("unexpected character %q at position %d", string(ch), i)
			}
			tokens = append(tokens, token{kind: tokString, text: text, pos: i})
			i = next
		}
	}
	tokens = append(tokens, token{kind: tokEOF, pos: len(expr)})
	return tokens, nil
}

func scanQuoted(expr string, start int) (string, int, error) {
	quote := expr[start]
	var b strings.Builder
	i := start + 1
	for i < len(expr) {
		ch := expr[i]
		if ch == '\\' && i+1 < len(expr) && quote == '"' {
			next := expr[i+1]
			switch next {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(next)
			}
			i += 2
			continue
		}
		if ch == quote {
			return b.String(), i + 1, nil
		}
		b.WriteByte(ch)
		i++
	}
	return "", 0, fmt.Errorf("unterminated string starting at position %d", start)
}

func scanVar(expr string, start int) (string, int, error) {
	i := start + 1
	braced := i < len(expr) && expr[i] == '{'
	if braced {
		i++
	}
	nameStart := i
	for i < len(expr) && isIdentChar(expr[i]) {
		i++
	}
	name := expr[nameStart:i]
	if name == "" {
		return "", 0, fmt.Errorf("expected a variable name after $ at position %d", start)
	}
	if braced {
		if i >= len(expr) || expr[i] != '}' {
			return "", 0, fmt.Errorf("unterminated ${...} at position %d", start)
		}
		i++
	}
	return name, i, nil
}

// scanBare reads an unquoted literal, which lets `$ENV == production` work without
// quotes. It stops at anything that could begin an operator.
func scanBare(expr string, start int) (string, int) {
	i := start
	for i < len(expr) {
		ch := expr[i]
		if ch == ' ' || ch == '\t' || ch == '(' || ch == ')' || ch == '!' ||
			ch == '=' || ch == '&' || ch == '|' || ch == '"' || ch == '\'' || ch == '$' {
			break
		}
		i++
	}
	return expr[start:i], i
}

func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// --- ast ---

type node interface {
	eval(env map[string]string) (bool, error)
}

type orNode struct{ left, right node }

func (n *orNode) eval(env map[string]string) (bool, error) {
	l, err := n.left.eval(env)
	if err != nil {
		return false, err
	}
	if l {
		return true, nil // short-circuit
	}
	return n.right.eval(env)
}

type andNode struct{ left, right node }

func (n *andNode) eval(env map[string]string) (bool, error) {
	l, err := n.left.eval(env)
	if err != nil {
		return false, err
	}
	if !l {
		return false, nil // short-circuit
	}
	return n.right.eval(env)
}

type notNode struct{ inner node }

func (n *notNode) eval(env map[string]string) (bool, error) {
	v, err := n.inner.eval(env)
	if err != nil {
		return false, err
	}
	return !v, nil
}

// operand is a variable reference or a literal.
type operand struct {
	isVar bool
	text  string
}

func (o operand) resolve(env map[string]string) string {
	if o.isVar {
		return env[o.text]
	}
	return o.text
}

type compareNode struct {
	left, right operand
	kind        tokenKind
	re          *regexp.Regexp
}

func (n *compareNode) eval(env map[string]string) (bool, error) {
	l := n.left.resolve(env)
	switch n.kind {
	case tokEq:
		return l == n.right.resolve(env), nil
	case tokNe:
		return l != n.right.resolve(env), nil
	case tokMatch:
		return n.re.MatchString(l), nil
	case tokNotMatch:
		return !n.re.MatchString(l), nil
	default:
		return false, fmt.Errorf("unsupported comparison")
	}
}

// truthyNode is a bare value used as a condition. A value is true when it is
// non-empty and is not one of the conventional falsey spellings, which is what
// lets `if: $CI` read naturally.
type truthyNode struct{ value operand }

func (n *truthyNode) eval(env map[string]string) (bool, error) {
	return truthy(n.value.resolve(env)), nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off", "null", "nil":
		return false
	default:
		return true
	}
}

// --- parser ---

type condParser struct {
	tokens []token
	pos    int
}

func (p *condParser) peek() token { return p.tokens[p.pos] }
func (p *condParser) atEnd() bool { return p.peek().kind == tokEOF }
func (p *condParser) next() token { t := p.tokens[p.pos]; p.pos++; return t }
func (p *condParser) accept(k tokenKind) bool {
	if p.peek().kind == k {
		p.pos++
		return true
	}
	return false
}

func (p *condParser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept(tokOr) {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &orNode{left: left, right: right}
	}
	return left, nil
}

func (p *condParser) parseAnd() (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.accept(tokAnd) {
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &andNode{left: left, right: right}
	}
	return left, nil
}

func (p *condParser) parseUnary() (node, error) {
	if p.accept(tokNot) {
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &notNode{inner: inner}, nil
	}
	return p.parsePrimary()
}

func (p *condParser) parsePrimary() (node, error) {
	if p.accept(tokLParen) {
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(tokRParen) {
			return nil, fmt.Errorf("expected ) at position %d, got %s", p.peek().pos, p.peek().describe())
		}
		return inner, nil
	}

	left, err := p.parseOperand()
	if err != nil {
		return nil, err
	}

	switch p.peek().kind {
	case tokEq, tokNe:
		op := p.next()
		right, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return &compareNode{left: left, right: right, kind: op.kind}, nil
	case tokMatch, tokNotMatch:
		op := p.next()
		right, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		if right.isVar {
			return nil, fmt.Errorf("the right side of %s at position %d must be a literal pattern", op.text, op.pos)
		}
		re, err := regexp.Compile(right.text)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression %q: %w", right.text, err)
		}
		return &compareNode{left: left, right: right, kind: op.kind, re: re}, nil
	default:
		return &truthyNode{value: left}, nil
	}
}

func (p *condParser) parseOperand() (operand, error) {
	t := p.next()
	switch t.kind {
	case tokVar:
		return operand{isVar: true, text: t.text}, nil
	case tokString:
		return operand{text: t.text}, nil
	default:
		return operand{}, fmt.Errorf("expected a value at position %d, got %s", t.pos, t.describe())
	}
}
