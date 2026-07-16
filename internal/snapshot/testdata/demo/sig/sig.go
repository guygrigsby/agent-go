package sig

func Fetch(a int, b string, rest ...int) int {
	n := a
	for _, r := range rest {
		n += r
	}
	return n
}

func UseFetch() int {
	return Fetch(1, "x", 2, 3)
}

func SpreadFetch(nums []int) int {
	return Fetch(1, "x", nums...)
}
