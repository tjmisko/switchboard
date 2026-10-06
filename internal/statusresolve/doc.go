// Package statusresolve is the one pure status resolver (#96): it selects a
// root's status from evidence candidates by what each kind of evidence may do,
// whether it is about the tracked agent, and whether it is still fresh, and
// returns the decision with every losing candidate and the rule that beat it.
//
// It does no I/O and reads no clock: the caller passes now. Source fusion
// lives here, outside agentgraph.Reduce, which stays neutral.
package statusresolve
