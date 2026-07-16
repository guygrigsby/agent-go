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

// Config holds settings. Legacy is referenced nowhere, so remove_field's
// example can delete it.
type Config struct {
	Name   string
	Legacy bool
}

// Unused is referenced nowhere, so delete_decl's example can delete it.
func Unused() {}

// Scale multiplies v by f. TestScale (sig_test.go) is the table-driven test
// the set_test_case/remove_test_case examples address.
func Scale(v, f int) int { return v * f }

// FetchErr returns a value and an error, giving Run an error-producing call.
func FetchErr() (int, error) { return 42, nil }

// Run discards FetchErr's error; wrap_error's example rewrites that
// assignment to bind and check it.
func Run() (int, error) {
	n, _ := FetchErr()
	return n, nil
}
