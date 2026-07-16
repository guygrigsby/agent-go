package main

import (
	"slices"
	"testing"
)

func TestClassifyKinds(t *testing.T) {
	cases := []struct {
		subject string
		want    []string
	}{
		{"Rename MaxEntries to MaxQuotas", []string{"rename"}},
		{"refact(bsr): Rename const to denote interpolation", []string{"rename"}},
		{"Add context as an argument for cli methods", []string{"add-param"}},
		{"aws: pass cancelable context with aws calls", []string{"add-param"}},
		{"Add statusThreshold parameter to job.Run", []string{"add-param"}},
		{"Move helpers into internal/util", []string{"move"}},
		{"Fix flaky test", nil},
		{"Update dependencies", nil},
		{"Renamed foo, moved bar to baz", []string{"rename", "move"}},
	}
	for _, c := range cases {
		if got := classifyKinds(c.subject); !slices.Equal(got, c.want) {
			t.Errorf("classifyKinds(%q) = %v, want %v", c.subject, got, c.want)
		}
	}
}
