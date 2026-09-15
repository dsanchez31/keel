// Package doctrine parses, versions, evaluates and hot swaps doctrine packs.
//
// A pack is one artifact with two consumers: its constraints validate a plan
// before launch (gate 4), its rules drive reaction during execution. Both read
// the same compiled pack, so a rule is never written twice and the two can
// never disagree.
//
// The package is in the decision path. Everything here is a pure function over
// in-memory values: no I/O, no clock, no map iteration. Reading pack files from
// doctrine-packs/ happens at the edge, which hands the bytes to Parse.
package doctrine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// APIVersion is the only pack schema version this package reads.
const APIVersion = "keel.doctrine/v1"

// ErrInvalidPack reports a pack that does not parse, type check or validate.
var ErrInvalidPack = errors.New("doctrine: invalid pack")

// ErrUnknownPack reports a reference no registered pack answers to.
var ErrUnknownPack = errors.New("doctrine: unknown pack")

// Params are the pack's typed parameters. They are read by code (the
// allocator, plan feasibility) and by expressions (as doctrine.<name>), so a
// value like the battery reserve has exactly one source.
type Params struct {
	// BatteryReservePct is the battery percentage held back from every range
	// computation, in [0, 100].
	BatteryReservePct int `json:"battery_reserve_pct"`
}

// Document is the pack as written, normalised: ids sorted, tag sets sorted,
// expressions kept as source text. It is what the pack hash covers, so two
// files that differ only in comments, whitespace or list order are the same
// pack.
type Document struct {
	APIVersion  string          `json:"apiVersion"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Params      Params          `json:"params"`
	Roles       []RoleDoc       `json:"roles,omitempty"`
	Constraints []ConstraintDoc `json:"constraints,omitempty"`
	Rules       []RuleDoc       `json:"rules,omitempty"`
}

// RoleDoc is one role as written.
type RoleDoc struct {
	ID       string   `json:"id"`
	Requires []string `json:"requires"`
}

// ConstraintDoc is one constraint as written.
type ConstraintDoc struct {
	ID   string `json:"id"`
	Rule string `json:"rule"`
}

// RuleDoc is one reactive rule as written.
type RuleDoc struct {
	ID       string `json:"id"`
	When     string `json:"when"`
	Then     string `json:"then"`
	Priority int    `json:"priority"`
}

// Role names a capability requirement. Requires is a sorted set.
type Role struct {
	ID       string
	Requires []string
}

// Constraint is a compiled invariant, checked per agent.
type Constraint struct {
	ID   string
	Rule Expr
}

// Rule is a compiled reactive rule.
type Rule struct {
	ID       string
	When     Condition
	Then     []Action
	Priority int
}

// Pack is a parsed, type-checked, content-addressed doctrine pack. Treat it as
// immutable: the evaluator and the hot swap share one Pack across ticks.
type Pack struct {
	Ref    domain.DoctrineRef
	Hash   string // lowercase hex SHA-256 of the canonical encoding of Doc
	Doc    Document
	Params Params
	Roles  []Role // sorted by id
	// Constraints are sorted by id, which is the order violations are
	// reported in.
	Constraints []Constraint
	// Rules are sorted by priority descending, then id ascending: the order
	// spec.md section 6.4 resolves them in.
	Rules []Rule
}

// Constraint returns the constraint with the given id.
func (p *Pack) Constraint(id string) (Constraint, bool) {
	i, ok := slices.BinarySearchFunc(p.Constraints, id, func(c Constraint, id string) int {
		return strings.Compare(c.ID, id)
	})
	if !ok {
		return Constraint{}, false
	}
	return p.Constraints[i], true
}

// Rule returns the rule with the given id.
func (p *Pack) Rule(id string) (Rule, bool) {
	for _, r := range p.Rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// The YAML shape. Expressions are decoded as nodes so an error can name the
// line it came from. Required scalars are pointers so absence is detectable.
type rawPack struct {
	APIVersion  string          `yaml:"apiVersion"`
	Name        string          `yaml:"name"`
	Version     string          `yaml:"version"`
	Params      *rawParams      `yaml:"params"`
	Roles       []rawRole       `yaml:"roles"`
	Constraints []rawConstraint `yaml:"constraints"`
	Rules       []rawRule       `yaml:"rules"`
}

type rawParams struct {
	BatteryReservePct *int `yaml:"battery_reserve_pct"`
}

type rawRole struct {
	ID       string   `yaml:"id"`
	Requires []string `yaml:"requires"`
}

type rawConstraint struct {
	ID   string    `yaml:"id"`
	Rule yaml.Node `yaml:"rule"`
}

type rawRule struct {
	ID       string    `yaml:"id"`
	When     yaml.Node `yaml:"when"`
	Then     yaml.Node `yaml:"then"`
	Priority *int      `yaml:"priority"`
}

// Parse reads one pack from YAML.
//
// The schema is closed: an unknown field, a second YAML document or a
// duplicate key is an error. Every expression and action list is compiled and
// type checked here, so a pack that parses cannot fail at evaluation.
func Parse(src []byte) (*Pack, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	var raw rawPack
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: empty document", ErrInvalidPack)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidPack, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: a pack is a single YAML document", ErrInvalidPack)
	}
	return build(raw)
}

func build(raw rawPack) (*Pack, error) {
	fail := func(format string, args ...any) (*Pack, error) {
		return nil, fmt.Errorf("%w: "+format, append([]any{ErrInvalidPack}, args...)...)
	}

	if raw.APIVersion != APIVersion {
		return fail("apiVersion %q, want %q", raw.APIVersion, APIVersion)
	}
	ref := domain.DoctrineRef{Name: raw.Name, Version: raw.Version}
	if err := ValidateRef(ref); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPack, err)
	}
	if raw.Params == nil || raw.Params.BatteryReservePct == nil {
		return fail("params.battery_reserve_pct is required")
	}
	params := Params{BatteryReservePct: *raw.Params.BatteryReservePct}
	if params.BatteryReservePct < 0 || params.BatteryReservePct > 100 {
		return fail("params.battery_reserve_pct %d is outside [0, 100]", params.BatteryReservePct)
	}

	doc := Document{APIVersion: raw.APIVersion, Name: raw.Name, Version: raw.Version, Params: params}
	pack := &Pack{Ref: ref, Params: params}
	ids := map[string]string{} // id -> "role", "constraint" or "rule"
	claim := func(kind, id string) error {
		if !ValidIdent(id) {
			return fmt.Errorf("%w: %s id %q is not lowercase kebab-case", ErrInvalidPack, kind, id)
		}
		if prev, dup := ids[id]; dup {
			return fmt.Errorf("%w: id %q is used by a %s and a %s", ErrInvalidPack, id, prev, kind)
		}
		ids[id] = kind
		return nil
	}

	for _, r := range raw.Roles {
		if err := claim("role", r.ID); err != nil {
			return nil, err
		}
		if len(r.Requires) == 0 {
			return fail("roles[%s].requires is empty", r.ID)
		}
		for _, tag := range r.Requires {
			if strings.TrimSpace(tag) == "" {
				return fail("roles[%s].requires holds an empty tag", r.ID)
			}
		}
		caps := domain.Capabilities{Tags: slices.Clone(r.Requires)}
		caps.SortTags()
		pack.Roles = append(pack.Roles, Role{ID: r.ID, Requires: caps.Tags})
		doc.Roles = append(doc.Roles, RoleDoc{ID: r.ID, Requires: slices.Clone(caps.Tags)})
	}

	for _, c := range raw.Constraints {
		if err := claim("constraint", c.ID); err != nil {
			return nil, err
		}
		src, err := scalar(c.Rule, "constraints", c.ID, "rule")
		if err != nil {
			return nil, err
		}
		expr, err := compileConstraint(src)
		if err != nil {
			return nil, located(err, "constraints", c.ID, "rule", c.Rule)
		}
		pack.Constraints = append(pack.Constraints, Constraint{ID: c.ID, Rule: expr})
		doc.Constraints = append(doc.Constraints, ConstraintDoc{ID: c.ID, Rule: src})
	}
	slices.SortFunc(pack.Constraints, func(a, b Constraint) int { return strings.Compare(a.ID, b.ID) })
	constraintIDs := make([]string, len(pack.Constraints))
	for i, c := range pack.Constraints {
		constraintIDs[i] = c.ID
	}

	for _, r := range raw.Rules {
		if err := claim("rule", r.ID); err != nil {
			return nil, err
		}
		if r.Priority == nil {
			return fail("rules[%s].priority is required", r.ID)
		}
		when, err := scalar(r.When, "rules", r.ID, "when")
		if err != nil {
			return nil, err
		}
		then, err := scalar(r.Then, "rules", r.ID, "then")
		if err != nil {
			return nil, err
		}
		cond, err := compileCondition(when, constraintIDs)
		if err != nil {
			return nil, located(err, "rules", r.ID, "when", r.When)
		}
		actions, err := compileActions(then)
		if err != nil {
			return nil, located(err, "rules", r.ID, "then", r.Then)
		}
		pack.Rules = append(pack.Rules, Rule{ID: r.ID, When: cond, Then: actions, Priority: *r.Priority})
		doc.Rules = append(doc.Rules, RuleDoc{ID: r.ID, When: when, Then: then, Priority: *r.Priority})
	}

	slices.SortFunc(pack.Roles, func(a, b Role) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(pack.Rules, compareRules)
	slices.SortFunc(doc.Roles, func(a, b RoleDoc) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(doc.Constraints, func(a, b ConstraintDoc) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(doc.Rules, func(a, b RuleDoc) int { return strings.Compare(a.ID, b.ID) })

	hash, err := eventlog.HexOf(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: hashing: %w", ErrInvalidPack, err)
	}
	pack.Doc = doc
	pack.Hash = hash
	return pack, nil
}

// compareRules is the resolution order: priority descending, then id
// ascending.
func compareRules(a, b Rule) int {
	if a.Priority != b.Priority {
		if a.Priority > b.Priority {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}

// scalar extracts a required, non-empty string from a YAML node.
func scalar(n yaml.Node, section, id, field string) (string, error) {
	if n.Kind == 0 {
		return "", fmt.Errorf("%w: %s[%s].%s is required", ErrInvalidPack, section, id, field)
	}
	if n.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%w: %s[%s].%s (line %d) must be a string", ErrInvalidPack, section, id, field, n.Line)
	}
	if strings.TrimSpace(n.Value) == "" {
		return "", fmt.Errorf("%w: %s[%s].%s (line %d) is empty", ErrInvalidPack, section, id, field, n.Line)
	}
	return n.Value, nil
}

// located wraps an expression error with where in the file it came from.
func located(err error, section, id, field string, n yaml.Node) error {
	return fmt.Errorf("%w: %s[%s].%s (line %d): %w", ErrInvalidPack, section, id, field, n.Line, err)
}

// Registry is the set of packs a server knows, the resolution target of a
// Plan IR doctrine field and of a hot swap request.
type Registry struct {
	packs []*Pack // sorted by name, then version precedence
}

// NewRegistry builds a registry. Two packs with the same name and version are
// an error even when their content is identical: a reference must name one
// pack, and silently keeping either would hide a packaging mistake.
func NewRegistry(packs ...*Pack) (*Registry, error) {
	sorted := slices.Clone(packs)
	slices.SortFunc(sorted, func(a, b *Pack) int {
		if c := strings.Compare(a.Ref.Name, b.Ref.Name); c != 0 {
			return c
		}
		return CompareVersions(a.Ref.Version, b.Ref.Version)
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Ref == sorted[i-1].Ref {
			return nil, fmt.Errorf("%w: %s registered twice", ErrInvalidPack, sorted[i].Ref)
		}
	}
	return &Registry{packs: sorted}, nil
}

// Resolve returns the pack a reference names. An unknown reference is an
// error that lists the valid alternatives, which is what gate 2 reports.
func (r *Registry) Resolve(ref domain.DoctrineRef) (*Pack, error) {
	for _, p := range r.packs {
		if p.Ref == ref {
			return p, nil
		}
	}
	valid := make([]string, len(r.packs))
	for i, p := range r.packs {
		valid[i] = p.Ref.String()
	}
	return nil, fmt.Errorf("%w: %s, available: %s", ErrUnknownPack, ref, listOrNone(valid))
}

// Refs lists every registered pack, sorted by name then version precedence.
func (r *Registry) Refs() []domain.DoctrineRef {
	out := make([]domain.DoctrineRef, len(r.packs))
	for i, p := range r.packs {
		out[i] = p.Ref
	}
	return out
}
