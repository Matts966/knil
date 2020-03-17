package nil // want package:"done"

import (
	"fmt"
	"math/rand"
)

type x struct{ f, g int }

func f(x, y *x) { // want f:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
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

func f2(ptr *[3]int, i interface{}) { // want f2:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	if ptr != nil {
		print(ptr[:])
		*ptr = [3]int{}
		print(*ptr)
	} else {
		print(ptr[:])   // want "nil dereference in slice operation"
		*ptr = [3]int{} // do not want "nil dereference in store" because root cause is ptr
		print(*ptr)     // do not want "nil dereference in load" because root cause is ptr

		if ptr != nil { // want "impossible condition: nil != nil"
			// Dominated by ptr==nil and ptr!=nil,
			// this block is unreachable.
			// We do not report errors within it.
			print(*ptr)
		}
	}

	if i != nil {
		print(i.(interface{ f() }))
	} else {
		print(i.(interface{ f() })) // want "nil dereference in type assertion"
	}
}

func g() error { // want g:"arguments: \\[\\], return value: \\[unknown\\], potential free variable: \\[\\]"
	if rand.Intn(10) > 5 {
		return nil
	}
	return fmt.Errorf("error")
}

func f3() error { // want f3:"arguments: \\[\\], return value: \\[unknown\\], potential free variable: \\[\\]"
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

func h(err error, b bool) { // want h:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	if err != nil && b {
		return
	} else if err != nil {
		panic(err)
	}
}

func i(x *int) error { // want i:"arguments: \\[nil\\], return value: \\[non-nil\\], potential free variable: \\[\\]"
	_ = *x // want "nil dereference in load"
	i(nil)
	for {
		if err := g(); err != nil {
			return err
		}
	}
}

func j(x *int) { // want j:"arguments: \\[non-nil\\], return value: \\[\\], potential free variable: \\[\\]"
	_ = *x
}

func k() { // want k:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	x := 0
	j(&x)
	x = 100
	j(&x)
}

func l(x *int) { // want l:"arguments: \\[unknown\\], return value: \\[\\], potential free variable: \\[\\]"
	_ = *x // want "nil dereference in load"
}

func m() { // want m:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	x := 0
	l(&x)
	l(nil)
}

func n() { // want n:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	var x interface{}
	_, _ = x.(error)
	_ = x.(error) // want "nil dereference in type assertion"
}

type s struct{}

func (v *s) m1() { // want m1:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[non-nil\\]"
	_ = *v // want "nil dereference in load"
}
func (v *s) m2() { // want m2:"arguments: \\[unknown\\], return value: \\[\\], potential free variable: \\[\\]"
	_ = *v // want "nil dereference in load"
}
func o() { // want o:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
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

func p() *int { // want p:"arguments: \\[\\], return value: \\[non-nil\\], potential free variable: \\[\\]"
	_ = *q() // want "nil dereference in load"
	x := 5
	return &x
}

func q() *int { // want q:"arguments: \\[\\], return value: \\[nil\\], potential free variable: \\[\\]"
	_ = *p()
	return nil
}

func r(i *int) { // want r:"arguments: \\[unknown\\], return value: \\[\\], potential free variable: \\[\\]"
	// TODO(Matts966): do not emit here because we already reported in sf.
	_ = *i // want "nil dereference in load"
}
func sf(i *int) { // want sf:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	_ = *i // want "nil dereference in load"
	r(i)
}

// T is an exported function and should care about the nilness
// of arguments.
func T(i *int) { // want T:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	_ = *i // want "nil dereference in load"
}

var keywords map[string]string // want keywords: "already reported global"
func v() { // want v:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	keywords = make(map[string]string)
	keywords["OK"] = "OK" // want "nil dereference in map update"
	// because global variable can be nil concurrently
	keywords["OK"] = "OK" // do not want "nil dereference in map update" because already reported
}

func w(i *int) {
	_ = *i
}
func x2(i *int) { // want x2:"arguments: \\[non-nil\\], return value: \\[\\], potential free variable: \\[\\]"
	w(i)
}
func y() { // want y:"arguments: \\[\\], return value: \\[\\], potential free variable: \\[\\]"
	i := 3
	x2(&i)
}
