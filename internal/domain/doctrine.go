package domain

// Window is the duration-window state of one (rule, agent) pair: the mission
// time its condition was first observed true, and whether the rule has
// already fired for this stretch of truth.
//
// An entry exists only while the condition holds. A tick where it is false
// drops the entry, which is what clears the window and re-arms the rule. It
// lives here rather than in doctrine because engine state carries it and a
// hot swap decision records it, and both speak domain types.
type Window struct {
	RuleID  string   `json:"rule_id"`
	Agent   VectorID `json:"agent"`
	SinceMs int64    `json:"since_ms"`
	Fired   bool     `json:"fired,omitempty"`
}

// DoctrineSwap records one hot swap: both packs by reference and by hash, and
// what happened to every duration window (spec.md section 6.6). It is carried
// by the doctrine_swap decision, so a replay reproduces the swap exactly and
// a reader can tell which windows kept their clock.
type DoctrineSwap struct {
	From     DoctrineRef `json:"from"`
	FromHash string      `json:"from_hash"`
	To       DoctrineRef `json:"to"`
	ToHash   string      `json:"to_hash"`
	// Retained are the windows carried into the new pack, sorted by rule id
	// then agent.
	Retained []Window `json:"retained,omitempty"`
	// Discarded are the windows of rules the new pack does not declare,
	// sorted by rule id then agent.
	Discarded []Window `json:"discarded,omitempty"`
}
