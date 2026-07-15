package main

import (
	"fmt"

	"demo/lib"
)

func main() {
	s := &lib.Store{}
	s.Put(lib.Double(lib.Limit))
	fmt.Println(s)
}
