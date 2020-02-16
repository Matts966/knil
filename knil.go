// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package nilness inspects the control-flow graph of an SSA function
// and reports errors such as nil pointer dereferences and degenerate
// nil pointer comparisons.
package knil

import (
	"fmt"
	"go/token"
	"go/types"
	"reflect"
	"unicode"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

const doc = `check for redundant or impossible nil comparisons completely and
nil pointer dereference soundly
`

var Analyzer = &analysis.Analyzer{
	Name:     "nilness",
	Doc:      doc,
	Run:      run,
	Requires: []*analysis.Analyzer{buildssa.Analyzer},
}

func run(pass *analysis.Pass) (interface{}, error) {
	ssainput := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	fns := ssainput.SrcFuncs
	for len(fns) > 0 {
		for _, fn := range fns {
			fns = checkFuncCall(pass, fn)
		}
	}
	for _, fn := range ssainput.SrcFuncs {
		runFunc(pass, fn)
	}
	return nil, nil
}

// panicArgs has the information about arguments which causes panic on
// calling the function when it is nil.
type panicArgs []nilness

func (*panicArgs) AFact() {}

// checkFuncCall checks all the function calls with nil
// parameters and export their information as ObjectFact.
func checkFuncCall(pass *analysis.Pass, fn *ssa.Function) []*ssa.Function {
	var updatedFunctions []*ssa.Function
	// visit visits reachable blocks of the CFG in dominance order,
	// maintaining a stack of dominating nilness facts.
	//
	// By traversing the dom tree, we can pop facts off the stack as
	// soon as we've visited a subtree.  Had we traversed the CFG,
	// we would need to retain the set of facts for each block.
	seen := make([]bool, len(fn.Blocks)) // seen[i] means visit should ignore block i
	var visit func(b *ssa.BasicBlock, stack []fact)
	visit = func(b *ssa.BasicBlock, stack []fact) {
		if seen[b.Index] {
			return
		}
		seen[b.Index] = true

		for _, instr := range b.Instrs {
			switch instr := instr.(type) {
			case ssa.CallInstruction:
				c := instr.Common()
				s := c.StaticCallee()
				if s == nil || s.Object() == nil {
					continue
				}

				f := c.StaticCallee().Object()
				if f.Pkg() != pass.Pkg {
					continue
				}

				var fact panicArgs
				pass.ImportObjectFact(f, &fact)
				if fact == nil {
					fact = nilnessOfS(stack, c.Args, isExported(s))
					pass.ExportObjectFact(f, &fact)
					updatedFunctions = append(updatedFunctions, s)
					continue
				}
				var updated bool
				if len(fact) == len(c.Args) {
					fact, updated = compareAndMerge(fact, nilnessOfS(stack, c.Args, isExported(s)))
				} else {
					pass.Reportf(instr.Pos(), "not consistent argments count function: %#v, arg1: %#v, arg2: %#v", s, instr.Common().Args, fact)
					continue
				}
				if updated {
					pass.ExportObjectFact(f, &fact)
					updatedFunctions = append(updatedFunctions, s)
				}
			}
		}

		// For nil comparison blocks, report an error if the condition
		// is degenerate, and push a nilness fact on the stack when
		// visiting its true and false successor blocks.
		if binop, tsucc, fsucc := eq(b); binop != nil {
			xnil := nilnessOf(stack, binop.X)
			ynil := nilnessOf(stack, binop.Y)

			if ynil != unknown && xnil != unknown && (xnil == isnil || ynil == isnil) {
				// If tsucc's or fsucc's sole incoming edge is impossible,
				// it is unreachable.  Prune traversal of it and
				// all the blocks it dominates.
				// (We could be more precise with full dataflow
				// analysis of control-flow joins.)
				var skip *ssa.BasicBlock
				if xnil == ynil {
					skip = fsucc
				} else {
					skip = tsucc
				}
				for _, d := range b.Dominees() {
					if d == skip && len(d.Preds) == 1 {
						continue
					}
					visit(d, stack)
				}
				return
			}

			// "if x == nil" or "if nil == y" condition; x, y are unknown.
			if xnil == isnil || ynil == isnil {
				var f fact
				if xnil == isnil {
					// x is nil, y is unknown:
					// t successor learns y is nil.
					f = fact{binop.Y, isnil}
				} else {
					// x is nil, y is unknown:
					// t successor learns x is nil.
					f = fact{binop.X, isnil}
				}

				for _, d := range b.Dominees() {
					// Successor blocks learn a fact
					// only at non-critical edges.
					// (We could do be more precise with full dataflow
					// analysis of control-flow joins.)
					s := stack
					if len(d.Preds) == 1 {
						if d == tsucc {
							s = append(s, f)
						} else if d == fsucc {
							s = append(s, f.negate())
						}
					}
					visit(d, s)
				}
				return
			}
		}

		for _, d := range b.Dominees() {
			visit(d, stack)
		}
	}

	// Visit the entry block.  No need to visit fn.Recover.
	if fn.Blocks != nil {
		visit(fn.Blocks[0], make([]fact, 0, 20)) // 20 is plenty
	}

	return updatedFunctions
}

func isExported(function *ssa.Function) bool {
	if function.Parent() != nil {
		return isExported(function.Parent())
	}

	name := function.Name()
	return unicode.IsUpper(rune(name[0]))
}

// func merge(a, b panicArgs) (panicArgs, bool) {
// 	if len(a) == len(b) {
// 		return compareAndMerge(a, b)
// 	}
// 	// varargs
// 	// if len(a) > len(b) {

// 	// } else {
// 	// 	longer = b
// 	// 	shorter = a
// 	// }
// 	// for i, s := range shorter {
// 	// 	new[i] = max(s, longer[i])
// 	// }
// }

// func shrink(p panicArgs, length int) panicArgs {
// 	x = panicArgs[length-1:]
// }

func compareAndMerge(a, b panicArgs) (panicArgs, bool) {
	new := make(panicArgs, len(a))
	if reflect.DeepEqual(a, b) {
		return a, false
	}
	for i, x := range a {
		new[i] = max(x, b[i])
	}
	return new, true
}

func max(a, b nilness) nilness {
	if a > b {
		return a
	}
	return b
}

func runFunc(pass *analysis.Pass, fn *ssa.Function) {
	reportf := func(category string, pos token.Pos, format string, args ...interface{}) {
		pass.Report(analysis.Diagnostic{
			Pos:      pos,
			Category: category,
			Message:  fmt.Sprintf(format, args...),
		})
	}

	// notNil reports an error if v is provably nil.
	notNil := func(stack []fact, instr ssa.Instruction, v ssa.Value, descr string) {
		nn := nilnessOf(stack, v)
		if nn != isnonnil {
			reportf("nilderef", instr.Pos(), "nil dereference in "+descr)
		}
	}

	// visit visits reachable blocks of the CFG in dominance order,
	// maintaining a stack of dominating nilness facts.
	//
	// By traversing the dom tree, we can pop facts off the stack as
	// soon as we've visited a subtree.  Had we traversed the CFG,
	// we would need to retain the set of facts for each block.
	seen := make([]bool, len(fn.Blocks)) // seen[i] means visit should ignore block i
	var visit func(b *ssa.BasicBlock, stack []fact)
	visit = func(b *ssa.BasicBlock, stack []fact) {
		if seen[b.Index] {
			return
		}
		seen[b.Index] = true

		// Report nil dereferences.
		for _, instr := range b.Instrs {
			switch instr := instr.(type) {
			case ssa.CallInstruction:
				notNil(stack, instr, instr.Common().Value,
					instr.Common().Description())
			case *ssa.FieldAddr:
				notNil(stack, instr, instr.X, "field selection")
			case *ssa.IndexAddr:
				notNil(stack, instr, instr.X, "index operation")
			case *ssa.MapUpdate:
				notNil(stack, instr, instr.Map, "map update")
			case *ssa.Slice:
				// A nilcheck occurs in ptr[:] iff ptr is a pointer to an array.
				if _, ok := instr.X.Type().Underlying().(*types.Pointer); ok {
					notNil(stack, instr, instr.X, "slice operation")
				}
			case *ssa.Store:
				notNil(stack, instr, instr.Addr, "store")
			case *ssa.TypeAssert:
				notNil(stack, instr, instr.X, "type assertion")
			case *ssa.UnOp:
				if instr.Op == token.MUL { // *X
					notNil(stack, instr, instr.X, "load")
				}
			}
		}

		// For nil comparison blocks, report an error if the condition
		// is degenerate, and push a nilness fact on the stack when
		// visiting its true and false successor blocks.
		if binop, tsucc, fsucc := eq(b); binop != nil {
			xnil := nilnessOf(stack, binop.X)
			ynil := nilnessOf(stack, binop.Y)

			if ynil != unknown && xnil != unknown && (xnil == isnil || ynil == isnil) {
				// Degenerate condition:
				// the nilness of both operands is known,
				// and at least one of them is nil.
				var adj string
				if (xnil == ynil) == (binop.Op == token.EQL) {
					adj = "tautological"
				} else {
					adj = "impossible"
				}
				reportf("cond", binop.Pos(), "%s condition: %s %s %s", adj, xnil, binop.Op, ynil)

				// If tsucc's or fsucc's sole incoming edge is impossible,
				// it is unreachable.  Prune traversal of it and
				// all the blocks it dominates.
				// (We could be more precise with full dataflow
				// analysis of control-flow joins.)
				var skip *ssa.BasicBlock
				if xnil == ynil {
					skip = fsucc
				} else {
					skip = tsucc
				}
				for _, d := range b.Dominees() {
					if d == skip && len(d.Preds) == 1 {
						continue
					}
					visit(d, stack)
				}
				return
			}

			// "if x == nil" or "if nil == y" condition; x, y are unknown.
			if xnil == isnil || ynil == isnil {
				var f fact
				if xnil == isnil {
					// x is nil, y is unknown:
					// t successor learns y is nil.
					f = fact{binop.Y, isnil}
				} else {
					// x is nil, y is unknown:
					// t successor learns x is nil.
					f = fact{binop.X, isnil}
				}

				for _, d := range b.Dominees() {
					// Successor blocks learn a fact
					// only at non-critical edges.
					// (We could do be more precise with full dataflow
					// analysis of control-flow joins.)
					s := stack
					if len(d.Preds) == 1 {
						if d == tsucc {
							s = append(s, f)
						} else if d == fsucc {
							s = append(s, f.negate())
						}
					}
					visit(d, s)
				}
				return
			}
		}

		for _, d := range b.Dominees() {
			visit(d, stack)
		}
	}

	// Visit the entry block.  No need to visit fn.Recover.
	if fn.Blocks == nil {
		return
	}
	f := make([]fact, 0, 20)
	var pa panicArgs
	if fn.Object() != nil {
		pass.ImportObjectFact(fn.Object(), &pa)
	}
	if pa == nil {
		visit(fn.Blocks[0], f)
		return
	}
	for i, p := range fn.Params {
		f = append(f, fact{p, pa[i]})
	}
	visit(fn.Blocks[0], f)
}

// A fact records that a block is dominated
// by the condition v == nil or v != nil.
type fact struct {
	value   ssa.Value
	nilness nilness
}

func (f fact) negate() fact { return fact{f.value, -f.nilness} }

type nilness int

const (
	isnonnil         = -1
	unknown  nilness = 0
	isnil            = 1
)

var nilnessStrings = []string{"non-nil", "unknown", "nil"}

func (n nilness) String() string { return nilnessStrings[n+1] }

func nilnessOfS(stack []fact, vs []ssa.Value, isExported bool) []nilness {
	ns := make([]nilness, len(vs))
	for i, s := range vs {
		if isExported {
			ns[i] = max(unknown, nilnessOf(stack, s))
		} else {
			ns[i] = nilnessOf(stack, s)
		}
	}
	return ns
}

// nilnessOf reports whether v is definitely nil, definitely not nil,
// or unknown given the dominating stack of facts.
func nilnessOf(stack []fact, v ssa.Value) nilness {
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
