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
	"reflect"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

const doc = `check for redundant or impossible nil comparisons completely and
nil pointer dereference soundly
`

var Analyzer = &analysis.Analyzer{
	Name:      "knil",
	Doc:       doc,
	Run:       run,
	Requires:  []*analysis.Analyzer{buildssa.Analyzer},
	FactTypes: []analysis.Fact{new(functionInfo), new(pkgDone), new(alreadyReportedGlobal)},
}

func run(pass *analysis.Pass) (interface{}, error) {
	ssainput := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	alreadyReported := make(map[ssa.Instruction]struct{})
	for true {
		updated := false
		for _, fn := range ssainput.SrcFuncs {
			// TODO(Matts966): ignore these cases in the new driver.
			if isIgnoredFunction(fn) {
				continue
			}

			if checkFunc(pass, fn, true, alreadyReported) {
				fi := functionInfo{}
				if fn.Object() != nil {
					pass.ImportObjectFact(fn.Object(), &fi)
				}

				updated = true
			}
		}
		if !updated {
			break
		}
	}
	// TODO(Matts966): Create new driver to search all the imported packages.
	// We should create it in the golang.org/x/tools because some required tools
	// are in internal packages. Also we can't rely on facts of standard packages
	// in some drivers such as Bazel and Blaze.
	pass.ExportPackageFact(&pkgDone{})
	for _, fn := range ssainput.SrcFuncs {

		// TODO(Matts966): handle these cases in the new driver.
		if isIgnoredFunction(fn) {
			continue
		}

		checkFunc(pass, fn, false, alreadyReported)
	}
	return nil, nil
}

// checkFunc checks all the function calls with nil
// parameters and export their information as ObjectFact,
// and returns whether the fact is updated.
// If onlyCheck is true, checkFunc only checks functions and
// exports facts.
// Diagnostics are emitted using the facts if onlyCheck is false.
//
func checkFunc(pass *analysis.Pass, fn *ssa.Function, onlyCheck bool, alreadyReported map[ssa.Instruction]struct{}) bool {
	bs := fn.Blocks
	if bs == nil {
		return false
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

	generateStackFromKnownFacts := func(fo types.Object) []nilnessOfValue {
		pa := functionInfo{}
		stack := make([]nilnessOfValue, 0, 20) // 20 is plenty
		if fo != nil {
			pass.ImportObjectFact(fo, &pa)
		}
		if pa.na.length() == 0 {
			return stack
		}
		if len(fn.Params) == pa.na.length() {
			merged := mergePosToNilnesses(pa.na)
			for i, p := range fn.Params {
				stack = append(stack, nilnessOfValue{p, merged[i]})
			}
			return stack
		}
		if pa.na.length()-len(fn.Params) != 1 {
			panic("inconsistent arguments but not method closure")
		}
		// There should be a receiver argument.
		merged := mergePosToNilnesses(pa.na)
		stack = append(stack, nilnessOfValue{fn.FreeVars[0], merged[0]})
		for i, p := range fn.Params {
			stack = append(stack, nilnessOfValue{p, merged[i+1]})
		}
		return stack
	}

	// visit visits reachable blocks of the CFG in dominance order,
	// maintaining a stack of dominating nilness facts.
	//
	// By traversing the dom tree, we can pop facts off the stack as
	// soon as we've visited a subtree.  Had we traversed the CFG,
	// we would need to retain the set of facts for each block.
	seen := make([]bool, len(bs)) // seen[i] means visit should ignore block i
	var visit visitor
	if onlyCheck {
		updated := false
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
					rns := nilnessesOf(stack, instr.Results)
					if len(fi.nr) == 0 {
						if len(rns) == 0 {
							continue
						}
						updated = true
						fi.nr = make(posToNilnesses)
						fi.nr[instr.Pos()] = rns
						pass.ExportObjectFact(fn.Object(), &fi)
						continue
					}
					if ns, ok := fi.nr[instr.Pos()]; ok {
						if reflect.DeepEqual(ns, rns) {
							continue
						}
					}
					updated = true
					fi.nr[instr.Pos()] = rns
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
							updated = true
							continue
						}
						fi := functionInfo{}
						pass.ImportObjectFact(f, &fi)
						switch fi.nr.length() {
						case 0:
							continue
						case 1:
							if v, ok := instr.(ssa.Value); ok {
								stack = append(stack, nilnessOfValue{v, mergePosToNilnesses(fi.nr)[0]})
							}
							continue
						default:
							if v, ok := instr.(ssa.Value); ok {
								vrs := v.Referrers()
								if vrs == nil {
									continue
								}
								c := 0
								merged := mergePosToNilnesses(fi.nr)
								for _, vr := range *vrs {
									if e, ok := vr.(*ssa.Extract); ok {
										stack = append(stack, nilnessOfValue{e, merged[c]})
										c++
									}
								}
							}
							continue
						}
					}

					fact := functionInfo{}
					pass.ImportObjectFact(f, &fact)
					if len(fact.na) == 0 && len(fact.rfv) == 0 {
						fact.na = make(posToNilnesses)
						fact.rfv = make(posToNilness)
						fact.na[instr.Pos()] = nilnessesOf(stack, c.Args)
						if len(s.FreeVars) > 0 {
							// Assume the receiver arguments are the first elements of FreeVars.
							fact.rfv[instr.Pos()] = nilnessOf(stack, s.FreeVars[0])
						}
						if len(fact.na) != 0 || len(fact.rfv) != 0 {
							updated = true
							pass.ExportObjectFact(f, &fact)
						}
						continue
					}
					if fact.na.length() == len(c.Args) {
						if fact.na.length() == 0 {
							continue
						}
						if na, ok := fact.na[instr.Pos()]; ok {
							if reflect.DeepEqual(na, nilnessesOf(stack, c.Args)) {
								if len(s.FreeVars) == 0 {
									continue
								}
								if rfv, ok := fact.rfv[instr.Pos()]; ok {
									if rfv == nilnessOf(stack, s.FreeVars[0]) {
										continue
									}
									updated = true
									fact.rfv[instr.Pos()] = nilnessOf(stack, s.FreeVars[0])
									pass.ExportObjectFact(f, &fact)
									continue
								}
							}
							updated = true
							fact.na[instr.Pos()] = nilnessesOf(stack, c.Args)
							if len(s.FreeVars) > 0 {
								fact.rfv[instr.Pos()] = nilnessOf(stack, s.FreeVars[0])
							}
							pass.ExportObjectFact(f, &fact)
							continue
						}
						updated = true
						fact.na[instr.Pos()] = nilnessesOf(stack, c.Args)

						if len(s.FreeVars) > 0 {
							fact.rfv[instr.Pos()] = nilnessOf(stack, s.FreeVars[0])
						}
						pass.ExportObjectFact(f, &fact)
						continue
					}
					if math.Abs(float64(fact.na.length()-len(c.Args))) != 1 {
						panic("inconsistent arguments but not method closure")
					}

					newFact := fact
					nnavwfv := nilnessesOf(stack, c.Args)
					if fact.na.length() > len(c.Args) {
						newFact.na[instr.Pos()] = append(nilnesses{nilnessOf(stack, s.FreeVars[0])}, nnavwfv...)
					} else {
						for pos, na := range fact.na {
							newFact.na[pos] = append(nilnesses{fact.rfv[pos]}, na...)
						}
						newFact.na[instr.Pos()] = nnavwfv
					}
					if reflect.DeepEqual(newFact, fact) {
						continue
					}
					updated = true
					pass.ExportObjectFact(f, &fact)
				}
			}

			if prune(b, stack, visit) {
				return
			}

			for _, d := range b.Dominees() {
				visit(d, stack)
			}
		}

		stack := generateStackFromKnownFacts(fn.Object())

		// Visit the entry block.  No need to visit fn.Recover.
		visit(bs[0], stack)
		return updated
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

				if v, ok := instr.(ssa.Value); ok && fi.nr.length() > 0 {
					vrs := v.Referrers()
					if vrs == nil {
						continue
					}
					c := 0
					merged := mergePosToNilnesses(fi.nr)
					for _, vr := range *vrs {
						switch i := vr.(type) {
						case *ssa.Extract:
							stack = append(stack, nilnessOfValue{i, merged[c]})
							c++
						// 1 value is returned.
						case ssa.Value:
							if fi.nr.length() != 1 {
								panic("inconsistent return values count")
							}
							stack = append(stack, nilnessOfValue{v, merged[0]})
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

	stack := generateStackFromKnownFacts(fn.Object())
	visit(bs[0], stack)
	return false
}
