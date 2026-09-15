package doctrine

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dsanchez31/keel/internal/domain"
)

// The expression language of spec.md section 6.2.
//
//	expr      := or_expr
//	or_expr   := and_expr ( "or" and_expr )*
//	and_expr  := cmp_expr ( "and" cmp_expr )*
//	cmp_expr  := sum ( ("=="|"!="|"<"|"<="|">"|">=") sum )
//	           | sum "within" sum
//	           | "violates" "(" ident ")"
//	sum       := operand ( ("+"|"-") operand )*
//	operand   := path | number | string | enum_literal
//	path      := ident ( "." ident )*
//	condition := expr ( "for" duration )?
//
// There are no parentheses, no negation, no user functions and no loops: an
// expression is a flat disjunction of conjunctions of comparisons, so parsing
// recurses to a fixed depth and evaluation touches each node once. The
// language runs inside the decision path and must terminate in bounded time.
//
// Every expression is type checked when its pack is parsed, against the fixed
// vocabulary below. A pack that loads cannot fail at evaluation.

// kind is the static type of a value.
type kind uint8

const (
	kindNumber kind = iota + 1
	kindString
	kindEnum
	kindPosition
	kindArea
)

func (k kind) String() string {
	switch k {
	case kindNumber:
		return "a number"
	case kindString:
		return "a string"
	case kindEnum:
		return "an enum"
	case kindPosition:
		return "a position"
	case kindArea:
		return "an area"
	default:
		return "untyped"
	}
}

// valueType is a kind plus, for an enum, the enum it belongs to.
type valueType struct {
	kind kind
	enum string
}

func (t valueType) String() string {
	if t.kind == kindEnum && t.enum != "" {
		return "an enum (" + t.enum + ")"
	}
	return t.kind.String()
}

// Enum names, as they appear in type errors.
const (
	enumDomain       = "domain"
	enumLinkState    = "link_state"
	enumMode         = "mode"
	enumMissionState = "mission_state"
)

// enumMembers lists the lowercase values of each enum, sorted. They are the
// domain constants themselves, so an enum literal cannot drift from the value
// it is compared with.
var enumMembers = map[string][]string{
	enumDomain: sortedStrings(string(domain.DomainAerial), string(domain.DomainGround)),
	enumLinkState: sortedStrings(
		string(domain.LinkOK), string(domain.LinkDegraded), string(domain.LinkLost)),
	enumMode: sortedStrings(
		string(domain.ModeIdle), string(domain.ModeTransit), string(domain.ModeScanning),
		string(domain.ModeRTB), string(domain.ModeDown)),
	enumMissionState: sortedStrings(
		string(domain.MissionPlanning), string(domain.MissionAwaitingApproval),
		string(domain.MissionRunning), string(domain.MissionComplete), string(domain.MissionFailed)),
}

// fieldID names one path of the vocabulary. The evaluator switches on it.
type fieldID uint8

const (
	fieldAgentID fieldID = iota + 1
	fieldAgentDomain
	fieldAgentBatteryPct
	fieldAgentRTBCostPct
	fieldAgentLinkState
	fieldAgentMode
	fieldAgentPosition
	fieldAgentSpeed
	fieldMissionArea
	fieldMissionCoveragePct
	fieldMissionState
	fieldLaneID
	fieldLaneIndex
	fieldLaneProgressPct
	fieldDoctrineBatteryReservePct
)

type fieldSpec struct {
	id  fieldID
	typ valueType
}

var (
	typeNumber   = valueType{kind: kindNumber}
	typeString   = valueType{kind: kindString}
	typePosition = valueType{kind: kindPosition}
	typeArea     = valueType{kind: kindArea}
)

func enumType(name string) valueType { return valueType{kind: kindEnum, enum: name} }

// vocabulary is every path an expression may read. The bound identifiers are
// agent (the vector under evaluation), mission, lane (the first lane the agent
// holds, by Index) and doctrine (the pack's own params).
var vocabulary = map[string]fieldSpec{
	"agent.id":                     {fieldAgentID, typeString},
	"agent.domain":                 {fieldAgentDomain, enumType(enumDomain)},
	"agent.battery_pct":            {fieldAgentBatteryPct, typeNumber},
	"agent.rtb_cost_pct":           {fieldAgentRTBCostPct, typeNumber},
	"agent.link_state":             {fieldAgentLinkState, enumType(enumLinkState)},
	"agent.mode":                   {fieldAgentMode, enumType(enumMode)},
	"agent.position":               {fieldAgentPosition, typePosition},
	"agent.speed":                  {fieldAgentSpeed, typeNumber},
	"mission.area":                 {fieldMissionArea, typeArea},
	"mission.coverage_pct":         {fieldMissionCoveragePct, typeNumber},
	"mission.state":                {fieldMissionState, enumType(enumMissionState)},
	"lane.id":                      {fieldLaneID, typeString},
	"lane.index":                   {fieldLaneIndex, typeNumber},
	"lane.progress_pct":            {fieldLaneProgressPct, typeNumber},
	"doctrine.battery_reserve_pct": {fieldDoctrineBatteryReservePct, typeNumber},
}

// boundRoots are the identifiers a path may start with, sorted.
var boundRoots = []string{"agent", "doctrine", "lane", "mission"}

// keywords may never be used as a path segment.
var keywords = []string{"and", "for", "or", "violates", "within"}

// Duration units of a "for" window, in milliseconds.
var durationUnits = map[string]int64{"ms": 1, "s": 1000, "m": 60_000}

// ExprError locates a problem in the source text of an expression.
type ExprError struct {
	Src string
	Pos int // byte offset into Src
	Msg string
}

func (e *ExprError) Error() string { return fmt.Sprintf("col %d: %s", e.Pos+1, e.Msg) }

// Expr is a compiled, type-checked boolean expression.
type Expr struct {
	src  string
	root boolNode
}

// String returns the source text the expression was compiled from.
func (e Expr) String() string { return e.src }

// Condition is a rule trigger: an expression and the mission time it must
// hold for continuously before the rule fires. A zero window fires on the
// tick the expression becomes true.
type Condition struct {
	Expr     Expr
	WindowMs int64
}

// boolOp discriminates boolNode.
type boolOp uint8

const (
	boolOr boolOp = iota + 1
	boolAnd
	boolCmp
	boolWithin
	boolViolates
)

// boolNode is one node of a compiled expression. It is a struct rather than an
// interface so the evaluator is one switch with no dynamic dispatch.
type boolNode struct {
	op         boolOp
	terms      []boolNode // boolOr, boolAnd
	cmp        string     // boolCmp: == != < <= > >=
	left       valueNode  // boolCmp, boolWithin (the position)
	right      valueNode  // boolCmp, boolWithin (the area)
	constraint string     // boolViolates
}

// valueOp discriminates valueNode.
type valueOp uint8

const (
	valPath valueOp = iota + 1
	valNumber
	valString
	valEnum
	valSum
)

// valueNode is an operand, or a sum of numeric operands.
type valueNode struct {
	op    valueOp
	typ   valueType
	pos   int
	text  string      // source text, for messages
	field fieldID     // valPath
	num   float64     // valNumber
	str   string      // valString; valEnum holds the lowercase member
	terms []valueNode // valSum
	neg   []bool      // valSum: neg[i] subtracts terms[i]; neg[0] is always false
}

// compileConstraint compiles a constraint's rule. A constraint holds or is
// violated at every tick, so it takes no window, and it cannot use violates:
// that keeps the language free of recursion.
func compileConstraint(src string) (Expr, error) {
	p, err := newParser(src, false, nil)
	if err != nil {
		return Expr{}, err
	}
	root, err := p.parseOr()
	if err != nil {
		return Expr{}, err
	}
	if t := p.peek(); p.isKeyword(t, "for") {
		return Expr{}, p.errAt(t.pos, "a constraint holds at every tick and takes no 'for' window")
	}
	if err := p.expectEOF(); err != nil {
		return Expr{}, err
	}
	return Expr{src: src, root: root}, nil
}

// compileCondition compiles a rule's when. constraints are the ids violates
// may name, sorted.
func compileCondition(src string, constraints []string) (Condition, error) {
	p, err := newParser(src, true, constraints)
	if err != nil {
		return Condition{}, err
	}
	root, err := p.parseOr()
	if err != nil {
		return Condition{}, err
	}
	var window int64
	if p.isKeyword(p.peek(), "for") {
		p.next()
		d := p.next()
		if d.kind != tokDuration {
			return Condition{}, p.errAt(d.pos, "duration expected after 'for', found %s", describe(d))
		}
		window, err = parseDuration(d)
		if err != nil {
			return Condition{}, &ExprError{Src: src, Pos: d.pos, Msg: err.Error()}
		}
	}
	if err := p.expectEOF(); err != nil {
		return Condition{}, err
	}
	return Condition{Expr: Expr{src: src, root: root}, WindowMs: window}, nil
}

// parseDuration reads "<integer><unit>", unit one of ms, s, m. The result must
// be positive and a whole number of ticks: a window that fell between two
// ticks would be rounded, and a rounded window fires at a time the pack does
// not say.
func parseDuration(t token) (int64, error) {
	i := 0
	for i < len(t.text) && isDigit(t.text[i]) {
		i++
	}
	digits, unit := t.text[:i], t.text[i:]
	scale, ok := durationUnits[unit]
	if !ok {
		return 0, fmt.Errorf("unknown duration unit %q in %q, use ms, s or m", unit, t.text)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > math.MaxInt64/scale {
		return 0, fmt.Errorf("duration %q is out of range", t.text)
	}
	ms := n * scale
	switch {
	case ms == 0:
		return 0, fmt.Errorf("duration %q is zero, omit 'for' for a rule that fires at once", t.text)
	case ms%domain.TickIntervalMs != 0:
		return 0, fmt.Errorf("duration %q is not a multiple of the %d ms tick", t.text, domain.TickIntervalMs)
	}
	return ms, nil
}

// Lexer.

type tokKind uint8

const (
	tokEOF tokKind = iota + 1
	tokIdent
	tokEnum
	tokNumber
	tokString
	tokDuration
	tokOp
	tokRaw // the verbatim argument of violates(...)
)

type token struct {
	kind tokKind
	text string // source text; the decoded value for tokString
	pos  int
	num  float64
}

func describe(t token) string {
	switch t.kind {
	case tokEOF:
		return "end of expression"
	case tokString:
		return strconv.Quote(t.text)
	default:
		return "'" + t.text + "'"
	}
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLower(c byte) bool  { return c >= 'a' && c <= 'z' }
func isUpper(c byte) bool  { return c >= 'A' && c <= 'Z' }
func isLetter(c byte) bool { return isLower(c) || isUpper(c) }
func isWord(c byte) bool   { return isLetter(c) || isDigit(c) || c == '_' }
func isSpace(c byte) bool  { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// twoCharOps are matched before single characters.
var twoCharOps = []string{"==", "!=", "<=", ">="}

const oneCharOps = "<>+-().,="

// lex splits src into tokens. It is shared by expressions and by the action
// lists of rules, which is why it knows about parentheses, commas and "=".
func lex(src string) ([]token, error) {
	if !utf8.ValidString(src) {
		return nil, &ExprError{Src: src, Msg: "source is not valid UTF-8"}
	}
	fail := func(pos int, format string, args ...any) ([]token, error) {
		return nil, &ExprError{Src: src, Pos: pos, Msg: fmt.Sprintf(format, args...)}
	}
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case isSpace(c):
			i++

		case isLetter(c):
			j := i
			for j < len(src) && isWord(src[j]) {
				j++
			}
			word := src[i:j]
			k, ok := classifyWord(word)
			if !ok {
				return fail(i, "%q is neither a lowercase path segment nor an UPPER_SNAKE enum literal", word)
			}
			toks = append(toks, token{kind: k, text: word, pos: i})
			i = j
			if word == "violates" {
				// The argument is a constraint id in kebab-case, which the
				// general lexer would split on '-'. It is taken verbatim.
				var err error
				if toks, i, err = lexRawArg(src, i, toks); err != nil {
					return nil, err
				}
			}

		case isDigit(c):
			j := i
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			if j < len(src) && src[j] == '.' {
				k := j + 1
				for k < len(src) && isDigit(src[k]) {
					k++
				}
				if k == j+1 {
					return fail(j, "digit expected after '.'")
				}
				j = k
			}
			if j < len(src) && isLetter(src[j]) {
				u := j
				for u < len(src) && isWord(src[u]) {
					u++
				}
				if strings.Contains(src[i:j], ".") {
					return fail(i, "duration %q must be a whole number", src[i:u])
				}
				toks = append(toks, token{kind: tokDuration, text: src[i:u], pos: i})
				i = u
				continue
			}
			n, err := strconv.ParseFloat(src[i:j], 64)
			if err != nil || math.IsInf(n, 0) {
				return fail(i, "number %q is out of range", src[i:j])
			}
			toks = append(toks, token{kind: tokNumber, text: src[i:j], pos: i, num: n})
			i = j

		case c == '"':
			s, next, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokString, text: s, pos: i})
			i = next

		default:
			matched := false
			for _, op := range twoCharOps {
				if strings.HasPrefix(src[i:], op) {
					toks = append(toks, token{kind: tokOp, text: op, pos: i})
					i += len(op)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			if strings.IndexByte(oneCharOps, c) >= 0 {
				toks = append(toks, token{kind: tokOp, text: string(c), pos: i})
				i++
				continue
			}
			r, _ := utf8.DecodeRuneInString(src[i:])
			return fail(i, "unexpected character %q", r)
		}
	}
	return append(toks, token{kind: tokEOF, pos: len(src)}), nil
}

// classifyWord accepts a lowercase identifier ([a-z][a-z0-9_]*) or an
// UPPER_SNAKE enum literal ([A-Z][A-Z0-9_]*). Mixed case is neither.
func classifyWord(w string) (tokKind, bool) {
	lower, upper := true, true
	for i := 0; i < len(w); i++ {
		c := w[i]
		if isUpper(c) {
			lower = false
		}
		if isLower(c) {
			upper = false
		}
	}
	switch {
	case lower && isLower(w[0]):
		return tokIdent, true
	case upper && isUpper(w[0]):
		return tokEnum, true
	default:
		return 0, false
	}
}

// lexRawArg reads "(" <anything but ')'> ")" after violates.
func lexRawArg(src string, i int, toks []token) ([]token, int, error) {
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i >= len(src) || src[i] != '(' {
		return nil, 0, &ExprError{Src: src, Pos: i, Msg: "'(' expected after violates"}
	}
	toks = append(toks, token{kind: tokOp, text: "(", pos: i})
	end := strings.IndexByte(src[i+1:], ')')
	if end < 0 {
		return nil, 0, &ExprError{Src: src, Pos: i, Msg: "unterminated violates("}
	}
	raw := src[i+1 : i+1+end]
	lead := len(raw) - len(strings.TrimLeft(raw, " \t\r\n"))
	toks = append(toks,
		token{kind: tokRaw, text: strings.TrimSpace(raw), pos: i + 1 + lead},
		token{kind: tokOp, text: ")", pos: i + 1 + end},
	)
	return toks, i + 2 + end, nil
}

// lexString reads a double-quoted string. The only escapes are \" and \\, so a
// string means exactly what it shows.
func lexString(src string, start int) (string, int, error) {
	var b strings.Builder
	i := start + 1
	for i < len(src) {
		c := src[i]
		switch c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 < len(src) && (src[i+1] == '"' || src[i+1] == '\\') {
				b.WriteByte(src[i+1])
				i += 2
				continue
			}
			return "", 0, &ExprError{Src: src, Pos: i, Msg: `invalid escape, only \" and \\ are allowed`}
		case '\n', '\r':
			return "", 0, &ExprError{Src: src, Pos: i, Msg: "newline in string"}
		}
		b.WriteByte(c)
		i++
	}
	return "", 0, &ExprError{Src: src, Pos: start, Msg: "unterminated string"}
}

// Parser.

type parser struct {
	src           string
	toks          []token
	i             int
	allowViolates bool
	constraints   []string // sorted
}

func newParser(src string, allowViolates bool, constraints []string) (*parser, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	return &parser{src: src, toks: toks, allowViolates: allowViolates, constraints: constraints}, nil
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *parser) errAt(pos int, format string, args ...any) error {
	return &ExprError{Src: p.src, Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) isKeyword(t token, kw string) bool { return t.kind == tokIdent && t.text == kw }

func (p *parser) isOp(t token, op string) bool { return t.kind == tokOp && t.text == op }

func (p *parser) expectEOF() error {
	if t := p.peek(); t.kind != tokEOF {
		return p.errAt(t.pos, "unexpected %s", describe(t))
	}
	return nil
}

func (p *parser) parseOr() (boolNode, error) {
	return p.parseChain("or", boolOr, p.parseAnd)
}

func (p *parser) parseAnd() (boolNode, error) {
	return p.parseChain("and", boolAnd, p.parseCmp)
}

// parseChain reads term ( kw term )*, collapsing a single term to itself.
func (p *parser) parseChain(kw string, op boolOp, term func() (boolNode, error)) (boolNode, error) {
	first, err := term()
	if err != nil {
		return boolNode{}, err
	}
	terms := []boolNode{first}
	for p.isKeyword(p.peek(), kw) {
		p.next()
		t, err := term()
		if err != nil {
			return boolNode{}, err
		}
		terms = append(terms, t)
	}
	if len(terms) == 1 {
		return first, nil
	}
	return boolNode{op: op, terms: terms}, nil
}

func (p *parser) parseCmp() (boolNode, error) {
	if p.isKeyword(p.peek(), "violates") {
		return p.parseViolates()
	}
	left, err := p.parseSum()
	if err != nil {
		return boolNode{}, err
	}
	op := p.peek()
	switch {
	case p.isKeyword(op, "within"):
		p.next()
		right, err := p.parseSum()
		if err != nil {
			return boolNode{}, err
		}
		return p.checkWithin(left, right)
	case op.kind == tokOp && isCmpOp(op.text):
		p.next()
		right, err := p.parseSum()
		if err != nil {
			return boolNode{}, err
		}
		return p.checkCmp(op, left, right)
	default:
		return boolNode{}, p.errAt(op.pos, "comparison expected after '%s', found %s", left.text, describe(op))
	}
}

func isCmpOp(s string) bool {
	switch s {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

func isOrdered(s string) bool { return s == "<" || s == "<=" || s == ">" || s == ">=" }

func (p *parser) parseViolates() (boolNode, error) {
	kw := p.next()
	if !p.allowViolates {
		return boolNode{}, p.errAt(kw.pos, "violates is only allowed in a rule's when, never in a constraint")
	}
	p.next() // "(", guaranteed by the lexer
	arg := p.next()
	p.next() // ")"
	if !ValidIdent(arg.text) {
		return boolNode{}, p.errAt(arg.pos, "violates takes a constraint id, found %q", arg.text)
	}
	if _, ok := slices.BinarySearch(p.constraints, arg.text); !ok {
		return boolNode{}, p.errAt(arg.pos, "unknown constraint %q, the pack declares: %s", arg.text, listOrNone(p.constraints))
	}
	return boolNode{op: boolViolates, constraint: arg.text}, nil
}

func (p *parser) parseSum() (valueNode, error) {
	first, err := p.parseOperand()
	if err != nil {
		return valueNode{}, err
	}
	if t := p.peek(); !p.isOp(t, "+") && !p.isOp(t, "-") {
		return first, nil
	}
	sum := valueNode{op: valSum, typ: typeNumber, pos: first.pos, terms: []valueNode{first}, neg: []bool{false}}
	for t := p.peek(); p.isOp(t, "+") || p.isOp(t, "-"); t = p.peek() {
		p.next()
		term, err := p.parseOperand()
		if err != nil {
			return valueNode{}, err
		}
		sum.terms = append(sum.terms, term)
		sum.neg = append(sum.neg, t.text == "-")
	}
	for _, term := range sum.terms {
		if term.op == valEnum {
			return valueNode{}, p.errAt(term.pos, "'%s' is an enum literal, + and - take numbers", term.text)
		}
		if term.typ.kind != kindNumber {
			return valueNode{}, p.errAt(term.pos, "'%s' is %s, + and - take numbers", term.text, term.typ)
		}
	}
	last := sum.terms[len(sum.terms)-1]
	sum.text = p.src[first.pos : last.pos+len(last.text)]
	return sum, nil
}

func (p *parser) parseOperand() (valueNode, error) {
	t := p.next()
	switch t.kind {
	case tokIdent:
		return p.parsePath(t)
	case tokNumber:
		return valueNode{op: valNumber, typ: typeNumber, pos: t.pos, text: t.text, num: t.num}, nil
	case tokString:
		return valueNode{op: valString, typ: typeString, pos: t.pos, text: strconv.Quote(t.text), str: t.text}, nil
	case tokEnum:
		// The enum it belongs to is resolved against the other operand.
		return valueNode{op: valEnum, typ: valueType{kind: kindEnum}, pos: t.pos, text: t.text, str: strings.ToLower(t.text)}, nil
	case tokDuration:
		return valueNode{}, p.errAt(t.pos, "duration %q is only valid after 'for'", t.text)
	default:
		return valueNode{}, p.errAt(t.pos, "operand expected, found %s", describe(t))
	}
}

func (p *parser) parsePath(first token) (valueNode, error) {
	segs := []string{first.text}
	end := first.pos + len(first.text)
	for p.isOp(p.peek(), ".") {
		p.next()
		seg := p.next()
		if seg.kind != tokIdent {
			return valueNode{}, p.errAt(seg.pos, "path segment expected after '.', found %s", describe(seg))
		}
		segs = append(segs, seg.text)
		end = seg.pos + len(seg.text)
	}
	for _, s := range segs {
		if slices.Contains(keywords, s) {
			return valueNode{}, p.errAt(first.pos, "operand expected, found keyword %q", s)
		}
	}
	path := strings.Join(segs, ".")
	if !slices.Contains(boundRoots, segs[0]) {
		return valueNode{}, p.errAt(first.pos, "unknown identifier %q, expressions read from %s", segs[0], strings.Join(boundRoots, ", "))
	}
	spec, ok := vocabulary[path]
	if !ok {
		return valueNode{}, p.errAt(first.pos, "unknown path %q, %s has: %s", path, segs[0], strings.Join(fieldsOf(segs[0]), ", "))
	}
	return valueNode{op: valPath, typ: spec.typ, pos: first.pos, text: p.src[first.pos:end], field: spec.id}, nil
}

// checkCmp types a comparison. Both sides must have the same type, an enum
// literal takes its enum from the path it faces, and the ordering operators
// take numbers only.
func (p *parser) checkCmp(op token, l, r valueNode) (boolNode, error) {
	if l.op == valEnum && r.op == valEnum {
		return boolNode{}, p.errAt(l.pos, "enum literal %s must be compared with an enum path", l.text)
	}
	if l.op == valEnum {
		if err := p.resolveEnum(&l, r); err != nil {
			return boolNode{}, err
		}
	}
	if r.op == valEnum {
		if err := p.resolveEnum(&r, l); err != nil {
			return boolNode{}, err
		}
	}
	if isOrdered(op.text) {
		for _, v := range []valueNode{l, r} {
			if v.typ.kind != kindNumber {
				return boolNode{}, p.errAt(v.pos, "'%s' is %s, %s compares numbers", v.text, v.typ, op.text)
			}
		}
		return boolNode{op: boolCmp, cmp: op.text, left: l, right: r}, nil
	}
	for _, v := range []valueNode{l, r} {
		if v.typ.kind == kindPosition || v.typ.kind == kindArea {
			return boolNode{}, p.errAt(v.pos, "'%s' is %s, which %s cannot compare; use within", v.text, v.typ, op.text)
		}
	}
	if l.typ != r.typ {
		msg := fmt.Sprintf("'%s' is %s and '%s' is %s", l.text, l.typ, r.text, r.typ)
		if hint := enumHint(l, r); hint != "" {
			msg += ", " + hint
		}
		return boolNode{}, p.errAt(op.pos, "%s", msg)
	}
	return boolNode{op: boolCmp, cmp: op.text, left: l, right: r}, nil
}

// resolveEnum gives an enum literal the type of the path it is compared with,
// after checking it is a member of that enum.
func (p *parser) resolveEnum(lit *valueNode, other valueNode) error {
	if other.typ.kind != kindEnum {
		return p.errAt(lit.pos, "enum literal %s compared with '%s', which is %s", lit.text, other.text, other.typ)
	}
	members := enumMembers[other.typ.enum]
	if _, ok := slices.BinarySearch(members, lit.str); !ok {
		return p.errAt(lit.pos, "%s is not a %s value, valid: %s", lit.text, other.typ.enum, strings.Join(upper(members), ", "))
	}
	lit.typ = other.typ
	return nil
}

// enumHint suggests the enum literal form when an enum path meets a string.
func enumHint(l, r valueNode) string {
	for _, pair := range [][2]valueNode{{l, r}, {r, l}} {
		if pair[0].typ.kind == kindEnum && pair[1].op == valString {
			return fmt.Sprintf("compare with an enum literal such as %s", strings.ToUpper(enumMembers[pair[0].typ.enum][0]))
		}
	}
	return ""
}

func (p *parser) checkWithin(l, r valueNode) (boolNode, error) {
	if l.typ.kind != kindPosition {
		return boolNode{}, p.errAt(l.pos, "'%s' is %s, within needs a position on its left", l.text, l.typ)
	}
	if r.typ.kind != kindArea {
		return boolNode{}, p.errAt(r.pos, "'%s' is %s, within needs an area on its right", r.text, r.typ)
	}
	return boolNode{op: boolWithin, left: l, right: r}, nil
}

// fieldsOf lists the fields under one bound root, sorted.
func fieldsOf(root string) []string {
	var out []string
	for _, path := range slices.Sorted(maps.Keys(vocabulary)) {
		if name, ok := strings.CutPrefix(path, root+"."); ok {
			out = append(out, name)
		}
	}
	return out
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func listOrNone(in []string) string {
	if len(in) == 0 {
		return "none"
	}
	return strings.Join(in, ", ")
}

func sortedStrings(in ...string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
