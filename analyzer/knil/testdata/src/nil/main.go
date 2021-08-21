// run -gcflags=-G=3
package main // want package:"done"

import (
	"fmt"
	"math/rand"
)

type x struct{ f, g int }

func f(x, y *x) {
	if x == nil {
		print(x.f) // want "nil dereference in field selection"
	} else {
		print(x.f)
	}

	if x == nil {
		if nil != y {
			print(1)
			panic(0)
		}
		x.f = 1 // do not want "nil dereference in field selection" because root cause is x
		y.f = 1 // want "nil dereference in field selection"
	}

	var f func()
	if f == nil { // want "tautological condition: nil == nil"
		go f() // want "nil dereference in dynamic function call"
	} else {
		// This block is unreachable,
		// so we don't report an error for the
		// nil dereference in the call.
		defer f()
	}
}

func f2(ptr *[3]int, i interface{}) {
	if ptr != nil {
		print(ptr[:])
		*ptr = [3]int{}
		_ = *ptr
	} else {
		print(ptr[:])   // want "nil dereference in slice operation"
		*ptr = [3]int{} // do not want "nil dereference in store" because root cause is ptr
		_ = *ptr     	// do not want "nil dereference in load" because root cause is ptr

		if ptr != nil { // want "impossible condition: nil != nil"
			// Dominated by ptr==nil and ptr!=nil,
			// this block is unreachable.
			// We do not report errors within it.
			_ = *ptr
		}
	}

	if i != nil {
		print(i.(interface{ f() }))
	} else {
		print(i.(interface{ f() })) // want "nil dereference in type assertion"
	}
}

func g() error { // want g:"arguments: map\\[[0-9]+:\\[\\]\\], return value: map\\[[0-9]+:\\[nil\\] [0-9]+:\\[unknown\\]\\], potential free variable: map\\[\\]"
	if rand.Intn(10) > 5 {
		return nil
	}
	return fmt.Errorf("error")
}

func f3() error { // want f3:"arguments: map\\[\\], return value: map\\[[0-9]+:\\[non-nil\\] [0-9]+:\\[nil\\]\\], potential free variable: map\\[\\]"
	err := g()
	if err != nil {
		return err
	}
	if err != nil && err.Error() == "foo" { // want "nil dereference in dynamic method call"
		print(0)
	}
	ch := make(chan int)
	if ch == nil { // want "impossible condition: non-nil == nil"
		print(0)
	}
	if ch != nil { // want "tautological condition: non-nil != nil"
		print(0)
	}
	return nil
}

func h(err error, b bool) {
	if err != nil && b {
		return
	} else if err != nil {
		panic(err)
	}
}

func i(x *int) error { // want i:"arguments: map\\[[0-9]+:\\[nil\\]\\], return value: map\\[[0-9]+:\\[non-nil\\]\\], potential free variable: map\\[\\]"
	_ = *x // want "nil dereference in load"
	i(nil)
	for {
		if err := g(); err != nil {
			return err
		}
	}
}

func j(x *int) { // want j:"arguments: map\\[[0-9]+:\\[non-nil\\] [0-9]+:\\[non-nil\\]\\], return value: map\\[\\], potential free variable: map\\[\\]"
	_ = *x
}

func k() {
	x := 0
	j(&x)
	x = 100
	j(&x)
}

func l(x *int) { // want l:"arguments: map\\[[0-9]+:\\[non-nil\\] [0-9]+:\\[nil\\]\\], return value: map\\[\\], potential free variable: map\\[\\]"
	_ = *x // want "nil dereference in load"
}

func m() {
	x := 0
	l(&x)
	l(nil)
}

func n() {
	var x interface{}
	_, _ = x.(error)
	_ = x.(error) // want "nil dereference in type assertion"
}

type s struct{}

func (v *s) m1() { // want m1:"arguments: map\\[[0-9]+:\\[\\]\\], return value: map\\[\\], potential free variable: map\\[[0-9]+:non-nil\\]"
	_ = *v // want "nil dereference in load"
}
func (v *s) m2() { // want m2:"arguments: map\\[[0-9]+:\\[non-nil\\] [0-9]+:\\[nil\\] [0-9]+:\\[non-nil\\] [0-9]+:\\[non-nil\\]\\], return value: map\\[\\], potential free variable: map\\[\\]"
	_ = *v // want "nil dereference in load"
}
func o() {
	s1 := s{}
	m1 := s1.m1
	var s2 *s
	m2 := s2.m1
	m1()
	m2()
	s1.m2()
	s2.m2()
	m1 = s1.m2
	m2 = s2.m2
	m1()
	m2()
}

func p() *int { // want p:"arguments: map\\[[0-9]+:\\[\\]\\], return value: map\\[[0-9]+:\\[non-nil\\]\\], potential free variable: map\\[\\]"
	_ = *q() // want "nil dereference in load"
	x := 5
	return &x
}

func q() *int { // want q:"arguments: map\\[[0-9]+:\\[\\]\\], return value: map\\[[0-9]+:\\[nil\\]\\], potential free variable: map\\[\\]"
	_ = *p()
	return nil
}

func r(i *int) { // want r:"arguments: map\\[[0-9]+:\\[unknown\\]\\], return value: map\\[\\], potential free variable: map\\[\\]"
	// TODO(Matts966): do not emit here because we already reported in sf.
	_ = *i // want "nil dereference in load"
}
func sf(i *int) {
	_ = *i // want "nil dereference in load"
	r(i)
}

// T is an exported function and should care about the nilness
// of arguments.
func T(i *int) {
	_ = *i // want "nil dereference in load"
}

var keywords map[string]string // want keywords: "already reported global"
func v() {
	keywords = make(map[string]string)
	keywords["OK"] = "OK" // want "nil dereference in map update"
	// because global variable can be nil concurrently
	keywords["OK"] = "OK" // do not want "nil dereference in map update" because already reported
}

func w(i *int) { // want w:"arguments: map\\[[0-9]+:\\[non-nil\\]\\], return value: map\\[\\], potential free variable: map\\[\\]"
	_ = *i // do not want "nil dereference in load" because the call of w is always with non-nil argument
}
func x2(i *int) { // want x2:"arguments: map\\[[0-9]+:\\[non-nil\\]\\], return value: map\\[\\], potential free variable: map\\[\\]"
	w(i) // do not want "nil dereference in load" because the call of x2 is always with non-nil argument
}
func y() {
	i := 3
	x2(&i)
}

type required[T any] interface {
  type T
}

func returnRequired[T required[int]]() T {
  return 3
}

func main() {
  println(returnRequired())
}

// type Nillable interface {
// }

// type Required[T any] struct {
//   Value T
// }

// type WithNonNilField struct {
//   NotNil Required[*int]
// }

// func AssignNilToRequiredField() {
//   x := WithNonNilField { NotNil: nil }
// }
