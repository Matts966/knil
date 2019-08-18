package main

func deref(i *interface{}) interface{} {
  return *i
}

func main() {
	var p *interface{}
  *p = 0 // want `p is nil, but will be dereferenced`

  deref(p)

	var i interface{}
	p = &i
	*p = 0 // OK
}
