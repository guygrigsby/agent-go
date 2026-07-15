package lib

func helper(v int) int { return v }

func UseHelper() int {
	h := func(v int) int { return v + 1 }
	_ = h
	return helper(2)
}
