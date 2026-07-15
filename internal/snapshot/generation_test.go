package snapshot

import "testing"

func TestGenerationBumpsOnMutation(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	g1 := s.generation("demo/lib", "Double")
	if g1 == 0 {
		t.Fatal("generation must be nonzero after load")
	}
	if _, err := s.SetBody("demo/lib", "Double", "return v + v"); err != nil {
		t.Fatal(err)
	}
	g2 := s.generation("demo/lib", "Double")
	if g2 <= g1 {
		t.Fatalf("generation did not advance: %d -> %d", g1, g2)
	}
	if got := s.generation("demo", "main"); got != 1 {
		t.Fatalf("main pkg generation moved without mutation: %d", got)
	}
}

func TestGenerationCheck(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	g := s.generation("demo/lib", "Double")
	if rej := s.checkGeneration("demo/lib", "Double", g); rej != nil {
		t.Fatalf("current generation rejected: %v", rej)
	}
	if rej := s.checkGeneration("demo/lib", "Double", g+1); rej == nil ||
		rej.Reason != "stale generation: re-view" {
		t.Fatalf("want stale reject, got %v", rej)
	}
	if rej := s.checkGeneration("demo/lib", "Double", 0); rej != nil {
		t.Fatalf("unspecified generation must pass: %v", rej)
	}
}
