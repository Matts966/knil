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
	"regexp"
	"unicode"

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

var ignoreFilesRegexp = `.*_test.go|zz_generated.*`

func run(pass *analysis.Pass) (interface{}, error) {
	ssainput := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	fns := setupMap(ssainput.SrcFuncs)
	for len(fns) > 0 {
		newfns := make(map[*ssa.Function]struct{}, len(fns))
		for fn := range fns {
			for _, f := range checkFunc(pass, fn) {
				newfns[f] = struct{}{}
			}
		}
		fns = newfns
	}
	pass.ExportPackageFact(&pkgDone{})
	for _, fn := range ssainput.SrcFuncs {
		if isIgnored(fn) {
			continue
		}
		alreadyReported := make(map[ssa.Instruction]struct{})
		runFunc(pass, fn, alreadyReported)
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

func isIgnored(v *ssa.Function) bool {
	m, err := regexp.MatchString(ignoreFilesRegexp, getFileName(v))
	if err != nil {
		panic(err)
	}
	return m
}

func getFileName(v *ssa.Function) string {
	fs := v.Prog.Fset
	return fs.File(v.Pos()).Name()
}

type functionInfo struct {
	// na has the information about arguments which can be nil.
	na   []nilness
	nr   []nilness
	rfvs []nilness
}

type receiverFreeVariables []nilness

func (*functionInfo) AFact() {}

type pkgDone struct{}

func (*pkgDone) AFact() {}

// checkFunc checks all the function calls with nil
// parameters and export their information as ObjectFact.
func checkFunc(pass *analysis.Pass, fn *ssa.Function) []*ssa.Function {
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
				if s == nil || s.Object() == nil || isExported(s) {
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
							stack = append(stack, fact{v, fi.nr[0]})
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
									stack = append(stack, fact{e, fi.nr[c]})
									c++
								}
							}
						}
						continue
					}
					continue
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

func compareAndMerge(prev, now []nilness) ([]nilness, bool) {
	if equal(prev, now) {
		return prev, false
	}
	var longer, shorter []nilness
	if len(prev) > len(now) {
		longer = prev
		shorter = now
	} else {
		longer = now
		shorter = prev
	}
	new := make([]nilness, len(longer))
	diff := len(longer) - len(shorter)
	for i, l := range longer {
		if i > diff-1 {
			new[i] = merge(l, shorter[i-diff])
		} else {
			new[i] = l
		}
	}
	if equal(prev, new) {
		return prev, false
	}
	return new, true
}

func mergeNilnesses(na, carg []nilness) ([]nilness, bool) {
	if len(na) != len(carg) {
		panic("inconsistent arguments count")
	}
	if equal(na, carg) {
		return na, false
	}
	nnn := make([]nilness, len(na))
	for i := range na {
		nnn[i] = merge(na[i], carg[i])
	}
	if equal(na, nnn) {
		return nnn, false
	}
	return nnn, true
}

func equal(a, b []nilness) bool {
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

func compressNilness(ns []nilness) nilness {
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

func runFunc(pass *analysis.Pass, fn *ssa.Function, alreadyReported map[ssa.Instruction]struct{}) {
	reportf := func(category string, pos token.Pos, format string, args ...interface{}) {
		pass.Report(analysis.Diagnostic{
			Pos:      pos,
			Category: category,
			Message:  fmt.Sprintf(format, args...),
		})
	}

	// notNil reports an error if v is provably nil.
	notNil := func(stack []fact, instr ssa.Instruction, v ssa.Value, descr string) {
		if nilnessOf(stack, v) == isnonnil {
			return
		}
		reportf("nilderef", instr.Pos(), "nil dereference in "+descr)
		// Only report root cause.
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
							stack = append(stack, fact{i, fi.nr[c]})
							c++
						// 1 value is returned.
						case ssa.Value:
							if len(fi.nr) != 1 {
								panic("inconsistent return values count")
							}
							stack = append(stack, fact{v, fi.nr[0]})
							break
						default:
							panic("function return values are referenced not as values")
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
	pa := functionInfo{}
	if fn.Object() != nil {
		pass.ImportObjectFact(fn.Object(), &pa)
	}
	if len(pa.na) == 0 {
		visit(fn.Blocks[0], f)
		return
	}
	if len(fn.Params) == len(pa.na) {
		for i, p := range fn.Params {
			f = append(f, fact{p, pa.na[i]})
		}
		visit(fn.Blocks[0], f)
		return
	}
	//print(len(pa.na), len(fn.Params))
	if len(pa.na)-len(fn.Params) != 1 {
		panic("inconsistent arguments but not method closure")
	}
	// There should be a receiver argument.
	f = append(f, fact{fn.FreeVars[0], pa.na[0]})
	for i, p := range fn.Params {
		f = append(f, fact{p, pa.na[i+1]})
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

var nilnessStrings = [...]string{"non-nil", "unknown", "nil"}

func (n nilness) String() string { return nilnessStrings[n+1] }

func nilnessesOf(stack []fact, vs []ssa.Value) []nilness {
	ns := make([]nilness, len(vs))
	for i, s := range vs {
		ns[i] = nilnessOf(stack, s)
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
