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

type Job interface {
	Run(a int) int
}

type job struct{}

func (job) Run(a int) int { return a }

func RunAll(j Job) int {
	return j.Run(1)
}

func NewJob() Job { return job{} }
