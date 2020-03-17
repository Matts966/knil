package knil

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

func mergeNilnesses(na, carg nilnesses) (nilnesses, bool) {
	if len(na) != len(carg) {
		panic("inconsistent arguments count")
	}
	if equal(na, carg) {
		return na, false
	}
	nnn := make(nilnesses, len(na))
	for i := range na {
		nnn[i] = merge(na[i], carg[i])
	}
	if equal(na, nnn) {
		return nnn, false
	}
	return nnn, true
}

func equal(a, b nilnesses) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

func merge(a, b nilness) nilness {
	if a*b == unknown || a != b {
		return unknown
	}
	return a
}

func compressNilness(ns nilnesses) nilness {
	// ns should have at least 1 element here
	// because if the count of arguments differs
	// there should be receivers.
	nv := ns[0]
	for _, n := range ns {
		if nv*n == unknown || nv != n {
			return unknown
		}
	}
	return nv
}

// A nilnessOfValue records that a block is dominated
// by the condition v == nil or v != nil.
type nilnessOfValue struct {
	value   ssa.Value
	nilness nilness
}

func (f nilnessOfValue) negate() nilnessOfValue { return nilnessOfValue{f.value, -f.nilness} }

type nilness int

type nilnesses []nilness

const (
	isnonnil         = -1
	unknown  nilness = 0
	isnil            = 1
)

var nilnessStrings = [...]string{"non-nil", "unknown", "nil"}

func (n nilness) String() string { return nilnessStrings[n+1] }

func nilnessesOf(stack []nilnessOfValue, vs []ssa.Value) nilnesses {
	ns := make(nilnesses, len(vs))
	for i, s := range vs {
		ns[i] = nilnessOf(stack, s)
	}
	return ns
}

// nilnessOf reports whether v is definitely nil, definitely not nil,
// or unknown given the dominating stack of facts.
func nilnessOf(stack []nilnessOfValue, v ssa.Value) nilness {
	// Is value intrinsically nil or non-nil?
	switch v := v.(type) {
	case *ssa.Alloc,
		*ssa.FieldAddr,
		*ssa.FreeVar,
		*ssa.Function,
		*ssa.Global,
		*ssa.IndexAddr,
		*ssa.MakeChan,
		*ssa.MakeClosure,
		*ssa.MakeInterface,
		*ssa.MakeMap,
		*ssa.MakeSlice,
		*ssa.Builtin:
		return isnonnil
	case *ssa.Const:
		if v.IsNil() {
			return isnil
		}
		return isnonnil
	}

	// Search dominating control-flow facts.
	for _, f := range stack {
		if f.value == v {
			return f.nilness
		}
	}
	return unknown
}

// If b ends with an equality comparison, eq returns the operation and
// its true (equal) and false (not equal) successors.
func eq(b *ssa.BasicBlock) (op *ssa.BinOp, tsucc, fsucc *ssa.BasicBlock) {
	if If, ok := b.Instrs[len(b.Instrs)-1].(*ssa.If); ok {
		if binop, ok := If.Cond.(*ssa.BinOp); ok {
			switch binop.Op {
			case token.EQL:
				return binop, b.Succs[0], b.Succs[1]
			case token.NEQ:
				return binop, b.Succs[1], b.Succs[0]
			}
		}
	}
	return nil, nil, nil
}
