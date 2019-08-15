package nil

func main() {
	var p *interface{}
	*p = 0 // want `p is nil, but will be dereferenced`

	var i interface{}
	p = &i
	*p = 0 // OK
}
