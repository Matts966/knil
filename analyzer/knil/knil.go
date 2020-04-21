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
	"sync"
	"unicode"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

const doc = `check for redundant or impossible nil comparisons completely and
nil pointer dereference soundly
`

// Main is the entrypoint for the analysis based on pointer analysis.
func Main(cg *callgraph.Graph) {
	passes := make([]*pass, len(cg.Root.Out))
	wg := &sync.WaitGroup{}
	for i, o := range cg.Root.Out {
		wg.Add(1)
		passes[i] = &pass{
			errs:  make([]*errorInfo, 0, 32),
			calls: make(callResults),
		}
		go analyzeNode(passes[i], wg, o.Callee)
	}
	wg.Wait()
	for _, pass := range passes {
		for _, err := range pass.errs {
			fmt.Println(err)
		}
	}
}

type pass struct {
	errs  []*errorInfo
	calls callResults
}

type errorInfo struct {
	stack []*callgraph.Edge
	err   error
}

type callResults = map[call][]*result

type call struct {
	id   int
	args string
}

type result struct {
	ret nilnesses
	err error
}

func (e *errorInfo) String() string {
	info := e.err.Error() + "\n"
	for _, s := range e.stack {
		info += s.Caller.Func.Name() + "->\n"
	}
	return info
}

func encodeCallInfo(n *callgraph.Node, args nilnesses) *call {
	return &call{
		id:   n.ID,
		args: fmt.Sprint(args),
	}
}

func possibleEdges(n *callgraph.Node, s *types.Signature) []*callgraph.Edge {
	pc := make([]*callgraph.Edge, 0, 4)
	for _, e := range n.Out {
		if types.AssignableTo(e.Callee.Func.Signature, s) {
			pc = append(pc, e)
		}
	}
	return pc
}

func analyzeNode(p *pass, wg *sync.WaitGroup, n *callgraph.Node) {
	callStack := make([]*callgraph.Edge, 0, 32)
	var call func(n *callgraph.Node, args nilnesses, cs []*callgraph.Edge) []*result
	call = func(n *callgraph.Node, args nilnesses, cs []*callgraph.Edge) []*result {
		ci := *encodeCallInfo(n, args)
		if ret, ok := p.calls[ci]; ok {
			return ret
		}

		notNil := func(stack []nilnessOfValue, instr ssa.Instruction, v ssa.Value, descr string) bool {
			if nilnessOf(stack, v) == isnonnil {
				return true
			}
			err := fmt.Errorf("nil dereference in " + descr)

			p.errs = append(p.errs, &errorInfo{
				stack: callStack,
				err:   err,
			})

			p.calls[ci] = append(p.calls[ci], &result{
				ret: nil,
				err: err,
			})

			return false
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

					// Skip appending errors because this analysis searches all
					// possible call graph.
					//
					// Degenerate condition:
					// the nilness of both operands is known,
					// and at least one of them is nil.
					// var adj string
					// if (xnil == ynil) == (binop.Op == token.EQL) {
					// 	adj = "tautological"
					// } else {
					// 	adj = "impossible"
					// }
					// // Only append error because it can not cause actual error
					// p.errs = append(p.errs, &errorInfo{
					// 	stack: nil,
					// 	// TODO(Matts966): also repost pos
					// 	err: fmt.Errorf(adj),
					// })

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
		seen := make([]bool, len(n.Func.Blocks)) // seen[i] means visit should ignore block i
		var visit visitor
		visit = func(b *ssa.BasicBlock, stack []nilnessOfValue) {
			if seen[b.Index] {
				return
			}
			seen[b.Index] = true

			// Report nil dereferences.
			for _, instr := range b.Instrs {
				switch instr := instr.(type) {
				case ssa.CallInstruction:
					c := instr.Common()
					notNil(stack, instr, c.Value, c.Description())

					// TODO(Matts966): handle bound and partially applied methods
					results := make([]*result, 0, 20)
					for _, e := range possibleEdges(n, c.Signature()) {
						callStack = append(callStack, e) // push
						results = append(results, call(n, nilnessesOf(stack, c.Args), callStack)...)
						callStack = callStack[:len(callStack)-1] // pop
					}

					if v, ok := instr.(ssa.Value); ok {
						vrs := v.Referrers()
						if vrs == nil {
							continue
						}
						var merged nilnesses = nil
						for _, r := range results {
							if r.ret != nil {
								if merged == nil {
									merged = r.ret
								} else {
									merged = mergeNilnesses(merged, r.ret)
								}
							}
						}
						if merged == nil {
							continue
						}
						for c, vr := range *vrs {
							switch i := vr.(type) {
							case *ssa.Extract:
								stack = append(stack, nilnessOfValue{i, merged[c]})
								c++
							// 1 value is returned.
							case ssa.Value:
								if len(merged) != 1 {
									panic("inconsistent return values count")
								}
								stack = append(stack, nilnessOfValue{v, merged[0]})
								break
							}
						}
					}
				case *ssa.Return:
					p.calls[ci] = append(p.calls[ci], &result{
						ret: nilnessesOf(stack, instr.Results),
						err: nil,
					})
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


		// TODO(Matts966): Handle method closure (partially applied receiver)
		stack := make([]nilnessOfValue, 0, 20) // 20 is plenty
		if len(n.Func.Params) == len(args) {
			for i, p := range n.Func.Params {
				stack = append(stack, nilnessOfValue{p, args[i]})
			}
		} else {
			// if len(n.Func.Params)-len(args) != 1 {
			// 	panic("inconsistent arguments but not method closure")
			// }
			// // There should be a receiver argument.
			// merged := mergePosToNilnesses(ptns)
			// stack = append(stack, nilnessOfValue{fn.FreeVars[0], merged[0]})

			// for i, p := range n.Func.Params[1:] {
			// 	stack = append(stack, nilnessOfValue{p, args[i]})
			// }
		}

		visit(n.Func.Blocks[0], stack)

		if _, ok := p.calls[ci]; !ok {
			p.calls[ci] = append(p.calls[ci], &result{
				ret: nil,
				err: nil,
			})
		}

		return p.calls[ci]
	}

	call(n, nilnesses{}, callStack)

	wg.Done()
}

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

	generateStackFromKnownFacts := func(ptns posToNilnesses) []nilnessOfValue {
		stack := make([]nilnessOfValue, 0, 20) // 20 is plenty
		if len(fn.Params) == ptns.length() {
			merged := mergePosToNilnesses(ptns)
			for i, p := range fn.Params {
				stack = append(stack, nilnessOfValue{p, merged[i]})
			}
			return stack
		}
		if ptns.length()-len(fn.Params) != 1 {
			panic("inconsistent arguments but not method closure")
		}
		// There should be a receiver argument.
		merged := mergePosToNilnesses(ptns)
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

		// Visit the entry block.  No need to visit fn.Recover.
		fo := fn.Object()
		if fo == nil {
			visit(bs[0], nil)
			return updated
		}
		pa := functionInfo{}
		pass.ImportObjectFact(fo, &pa)
		if pa.na.length() == 0 {
			visit(bs[0], nil)
			return updated
		}
		stack := generateStackFromKnownFacts(pa.na)
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

	// Visit the entry block.  No need to visit fn.Recover.
	fo := fn.Object()
	if fo == nil {
		visit(bs[0], nil)
		return false
	}
	pa := functionInfo{}
	pass.ImportObjectFact(fo, &pa)
	if pa.na.length() == 0 {
		// TODO(Matts966): Ignore not only unexported but also exported but not called functions.
		// TODO(Matts966): Add an option to always check exported functions.
		if isExported(fo.Name()) {
			visit(bs[0], nil)
		}
		// Do not check not called unexported functions.
		return false
	}
	stack := generateStackFromKnownFacts(pa.na)
	visit(bs[0], stack)
	return false
}

func isExported(fn string) bool {
	for _, f := range fn {
		if unicode.IsUpper(f) {
			return true
		}
	}
	return false
}
