// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package knil inspects the control-flow graph of an SSA function
// and reports errors such as nil pointer dereferences and degenerate
// nil pointer comparisons.
package knil

import (
	"fmt"
	"go/token"
	"go/types"
	"math"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

const doc = `check for redundant or impossible nil comparisons completely and
nil pointer dereference soundly
`

var Analyzer = &analysis.Analyzer{
	Name:     "knil",
	Doc:      doc,
	Run:      run,
	Requires: []*analysis.Analyzer{buildssa.Analyzer},
}

func run(pass *analysis.Pass) (interface{}, error) {
	ssainput := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	fns := setupMap(ssainput.SrcFuncs)
	alreadyReported := make(map[ssa.Instruction]struct{})
	for len(fns) > 0 {
		newfns := make(map[*ssa.Function]struct{}, len(fns))
		for fn := range fns {
			for _, f := range checkFunc(pass, fn, true, alreadyReported) {
				newfns[f] = struct{}{}
			}
		}
		fns = newfns
	}
	pass.ExportPackageFact(&pkgDone{})
	for _, fn := range ssainput.SrcFuncs {
		if isIgnoredFunction(fn) {
			continue
		}
		checkFunc(pass, fn, false, alreadyReported)
	}
	return nil, nil
}

func setupMap(fs []*ssa.Function) map[*ssa.Function]struct{} {
	ret := make(map[*ssa.Function]struct{}, len(fs))
	for _, f := range fs {
		ret[f] = struct{}{}
	}
	return ret
}

// checkFunc checks all the function calls with nil
// parameters and export their information as ObjectFact,
// and returns functions whose fact is updated.
// If onlyCheck is true, checkFunc only checks functions and
// exports facts.
// Diagnostics are emitted using the facts if onlyCheck is false.
//
func checkFunc(pass *analysis.Pass, fn *ssa.Function, onlyCheck bool, alreadyReported map[ssa.Instruction]struct{}) []*ssa.Function {
	if fn.Blocks == nil {
		return nil
	}

	reportf := func(category string, pos token.Pos, format string, args ...interface{}) {
		pass.Report(analysis.Diagnostic{
			Pos:      pos,
			Category: category,
			Message:  fmt.Sprintf(format, args...),
		})
	}

	type visitor func(b *ssa.BasicBlock, stack []nilnessOfValue)

	prune := func(b *ssa.BasicBlock, stack []nilnessOfValue, visit visitor) bool {
		// For nil comparison blocks, report an error if the condition
		// is degenerate, and push a nilness fact on the stack when
		// visiting its true and false successor blocks.
		if binop, tsucc, fsucc := eq(b); binop != nil {
			xnil := nilnessOf(stack, binop.X)
			ynil := nilnessOf(stack, binop.Y)

			if ynil != unknown && xnil != unknown && (xnil == isnil || ynil == isnil) {
				if !onlyCheck {
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
				}

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
				return true
			}

			// "if x == nil" or "if nil == y" condition; x, y are unknown.
			if xnil == isnil || ynil == isnil {
				var f nilnessOfValue
				if xnil == isnil {
					// x is nil, y is unknown:
					// t successor learns y is nil.
					f = nilnessOfValue{binop.Y, isnil}
				} else {
					// x is nil, y is unknown:
					// t successor learns x is nil.
					f = nilnessOfValue{binop.X, isnil}
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
				return true
			}
		}
		return false
	}

	// visit visits reachable blocks of the CFG in dominance order,
	// maintaining a stack of dominating nilness facts.
	//
	// By traversing the dom tree, we can pop facts off the stack as
	// soon as we've visited a subtree.  Had we traversed the CFG,
	// we would need to retain the set of facts for each block.
	seen := make([]bool, len(fn.Blocks)) // seen[i] means visit should ignore block i
	var visit visitor
	if onlyCheck {
		// updatedFunctions stores functions whose fact is updated.
		var updatedFunctions []*ssa.Function
		visit = func(b *ssa.BasicBlock, stack []nilnessOfValue) {
			if seen[b.Index] {
				return
			}
			seen[b.Index] = true

			for _, instr := range b.Instrs {
				switch instr := instr.(type) {
				case *ssa.Return:
					fi := functionInfo{}
					if fn.Object() == nil {
						continue
					}
					pass.ImportObjectFact(fn.Object(), &fi)
					if len(fi.nr) == 0 {
						fi.nr = nilnessesOf(stack, instr.Results)
						pass.ExportObjectFact(fn.Object(), &fi)
						continue
					}
					fi.nr, _ = mergeNilnesses(fi.nr, nilnessesOf(stack, instr.Results))
					pass.ExportObjectFact(fn.Object(), &fi)
					continue
				case ssa.CallInstruction:
					c := instr.Common()
					s := c.StaticCallee()
					if s == nil || s.Object() == nil {
						continue
					}

					f := s.Object()
					if f.Pkg() != pass.Pkg {
						if !pass.ImportPackageFact(f.Pkg(), &pkgDone{}) {
							updatedFunctions = append(updatedFunctions, fn)
							continue
						}
						fi := functionInfo{}
						pass.ImportObjectFact(f, &fi)
						switch len(fi.nr) {
						case 0:
							continue
						case 1:
							if v, ok := instr.(ssa.Value); ok {
								stack = append(stack, nilnessOfValue{v, fi.nr[0]})
							}
							continue
						default:
							if v, ok := instr.(ssa.Value); ok {
								vrs := v.Referrers()
								if vrs == nil {
									continue
								}
								c := 0
								for _, vr := range *vrs {
									if e, ok := vr.(*ssa.Extract); ok {
										stack = append(stack, nilnessOfValue{e, fi.nr[c]})
										c++
									}
								}
							}
							continue
						}
					}

					fact := functionInfo{}
					pass.ImportObjectFact(f, &fact)
					if len(fact.na) == 0 && len(fact.rfvs) == 0 {
						fact.na = nilnessesOf(stack, c.Args)
						if len(s.FreeVars) > 0 {
							// Assume the receiver arguments are the first elements of FreeVars.
							fact.rfvs = append(fact.rfvs, nilnessOf(stack, s.FreeVars[0]))
						}
						pass.ExportObjectFact(f, &fact)
						if len(fact.na) != 0 || len(fact.rfvs) != 0 {
							updatedFunctions = append(updatedFunctions, s)
						}
						continue
					}
					var updated bool
					if len(fact.na) == len(c.Args) {
						if len(fact.na) == 0 {
							continue
						}
						fact.na, updated = mergeNilnesses(fact.na, nilnessesOf(stack, c.Args))
						if len(s.FreeVars) > 0 {
							fact.rfvs = append(fact.rfvs, nilnessOf(stack, s.FreeVars[0]))
						}
					} else {
						if math.Abs(float64(len(fact.na)-len(c.Args))) != 1 {
							panic("inconsistent arguments but not method closure")
						}
						nnavwfv := nilnessesOf(stack, c.Args)
						if len(fact.na) > len(c.Args) {
							fact.na, updated = mergeNilnesses(fact.na, append([]nilness{nilnessOf(stack, s.FreeVars[0])}, nnavwfv...))
						} else {
							fact.na, updated = mergeNilnesses(append([]nilness{compressNilness(fact.rfvs)}, fact.na...), nnavwfv)
						}
					}
					if updated {
						pass.ExportObjectFact(f, &fact)
						updatedFunctions = append(updatedFunctions, s)
					}
				}
			}

			if prune(b, stack, visit) {
				return
			}

			for _, d := range b.Dominees() {
				visit(d, stack)
			}
		}

		// Visit the entry block.  No need to visit fn.Recover.
		visit(fn.Blocks[0], make([]nilnessOfValue, 0, 20)) // 20 is plenty

		return updatedFunctions
	}

	// onlyCheck is false, emit diagnostics

	// notNil reports an error if v can be nil.
	notNil := func(stack []nilnessOfValue, instr ssa.Instruction, v ssa.Value, descr string) {
		if nilnessOf(stack, v) == isnonnil {
			return
		}
		reportf("nilderef", instr.Pos(), "nil dereference in "+descr)

		// Only report root cause.

		// Global is always with register operation
		if u, ok := v.(*ssa.UnOp); ok {
			// Global does not hold referrers
			// so we export object facts.
			if g, ok := u.X.(*ssa.Global); ok && g.Pkg.Pkg == pass.Pkg {
				pass.ExportObjectFact(g.Object(), &alreadyReportedGlobal{})
				return
			}
		}

		vrs := v.Referrers()
		for vrs != nil {
			nvrs := make([]ssa.Instruction, 0, 16)
			for _, vr := range *vrs {
				if _, ok := alreadyReported[vr]; ok {
					continue
				}
				alreadyReported[vr] = struct{}{}
				if vrn, ok := vr.(ssa.Node); ok {
					vrnrs := vrn.Referrers()
					if vrnrs == nil {
						continue
					}
					nvrs = append(nvrs, *vrnrs...)
				}
			}
			if len(nvrs) == 0 {
				break
			}
			*vrs = nvrs
		}
	}

	visit = func(b *ssa.BasicBlock, stack []nilnessOfValue) {
		if seen[b.Index] {
			return
		}
		seen[b.Index] = true

		// Report nil dereferences.
		for _, instr := range b.Instrs {
			// Check if the operand is already reported
			// Global and skip if it is.
			var rands [10]*ssa.Value
			ios := instr.Operands(rands[:0])
			if len(ios) > 0 {
				// Checking the first operand is enough
				// because we only have to check
				// operatons with 1 operand.
				if u, ok := (*ios[0]).(*ssa.UnOp); ok {
					if g, ok := u.X.(*ssa.Global); ok {
						f := &alreadyReportedGlobal{}
						if pass.ImportObjectFact(g.Object(), f) {
							continue
						}
					}
				}
			}

			if _, ok := alreadyReported[instr]; ok {
				continue
			}
			switch instr := instr.(type) {
			case ssa.CallInstruction:
				notNil(stack, instr, instr.Common().Value,
					instr.Common().Description())

				s := instr.Common().StaticCallee()
				if s == nil {
					continue
				}
				fo := s.Object()
				if fo == nil {
					continue
				}

				fi := functionInfo{}
				pass.ImportObjectFact(fo, &fi)

				if v, ok := instr.(ssa.Value); ok && len(fi.nr) > 0 {
					vrs := v.Referrers()
					if vrs == nil {
						continue
					}
					c := 0
					for _, vr := range *vrs {
						switch i := vr.(type) {
						case *ssa.Extract:
							stack = append(stack, nilnessOfValue{i, fi.nr[c]})
							c++
						// 1 value is returned.
						case ssa.Value:
							if len(fi.nr) != 1 {
								panic("inconsistent return values count")
							}
							stack = append(stack, nilnessOfValue{v, fi.nr[0]})
							break
						}
					}
				}
			case *ssa.FieldAddr:

				notNil(stack, instr, instr.X, "field selection")
			// Currently we do not support check for index operations
			// because range for slice is not Range in SSA. Range in
			// SSA is only for map and string, and we can't distinguish
			// range based addressing, which is safe, and naive
			// addressing for nil, which cause an error. Also the error
			// is index out of range, not nil pointer dereference,
			//  even if the slice operand is nil.
			//
			// case *ssa.IndexAddr:
			// 	notNil(stack, instr, instr.X, "index operation")
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
				// Only the 1-result type assertion panics.
				//
				// _ = fp.(someType)
				if instr.CommaOk {
					continue
				}
				notNil(stack, instr, instr.X, "type assertion")
			case *ssa.UnOp:
				if instr.Op == token.MUL { // *X
					notNil(stack, instr, instr.X, "load")
				}
			}
		}

		if prune(b, stack, visit) {
			return
		}

		for _, d := range b.Dominees() {
			visit(d, stack)
		}
	}

	f := make([]nilnessOfValue, 0, 20)
	pa := functionInfo{}
	if fn.Object() != nil {
		pass.ImportObjectFact(fn.Object(), &pa)
	}
	if len(pa.na) == 0 {
		visit(fn.Blocks[0], f)
		return nil
	}
	if len(fn.Params) == len(pa.na) {
		for i, p := range fn.Params {
			f = append(f, nilnessOfValue{p, pa.na[i]})
		}
		visit(fn.Blocks[0], f)
		return nil
	}
	if len(pa.na)-len(fn.Params) != 1 {
		panic("inconsistent arguments but not method closure")
	}
	// There should be a receiver argument.
	f = append(f, nilnessOfValue{fn.FreeVars[0], pa.na[0]})
	for i, p := range fn.Params {
		f = append(f, nilnessOfValue{p, pa.na[i+1]})
	}
	visit(fn.Blocks[0], f)
	return nil
}
