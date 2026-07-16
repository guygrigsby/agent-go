package snapshot

import (
	"os"
	"testing"

	"golang.org/x/tools/go/packages"
)

// BenchmarkRetypecheck measures the incremental typecheck on a real
// workspace: the dirty set is one package plus its transitive reverse
// importers, the same set a rename there would recheck. Compare drivers:
//
//	AGO_TYPECHECK_REPO=~/.cache/ago-bench/clones/boundary \
//	AGO_TYPECHECK_PKG=github.com/hashicorp/boundary/internal/ratelimit \
//	go test ./internal/snapshot -bench Retypecheck -benchtime 5x -run '^$'
//
// with and without AGO_SERIAL_TYPECHECK=1.
func BenchmarkRetypecheck(b *testing.B) {
	dir := os.Getenv("AGO_TYPECHECK_REPO")
	pkg := os.Getenv("AGO_TYPECHECK_PKG")
	if dir == "" || pkg == "" {
		b.Skip("set AGO_TYPECHECK_REPO and AGO_TYPECHECK_PKG to benchmark on a real workspace")
	}
	s := New(dir)
	if _, err := s.Status(); err != nil {
		b.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.primary(pkg)
	if target == nil {
		b.Fatalf("package %s not loaded", pkg)
	}
	dirty := append([]*packages.Package{target}, s.affected(pkg)...)
	b.ResetTimer()
	for range b.N {
		diags, n, err := s.retypecheck(dirty, nil)
		if err != nil || len(diags) > 0 {
			b.Fatalf("retypecheck: %v %v", err, diags)
		}
		b.ReportMetric(float64(n), "pkgs")
	}
}
