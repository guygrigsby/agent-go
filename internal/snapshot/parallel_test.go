package snapshot

import (
	"encoding/json"
	"strings"
	"testing"
)

// The parallel retypecheck must produce byte-identical results to the
// serial driver: same accept/reject, same diagnostics in the same order.
// The fixture gets a seeded broken file so the diagnostic path is real.
func TestParallelRetypecheckMatchesSerial(t *testing.T) {
	run := func(serial bool) (string, string) {
		t.Setenv("AGO_SERIAL_TYPECHECK", map[bool]string{true: "1", false: ""}[serial])
		s := demo(t)
		if _, err := s.Status(); err != nil {
			t.Fatal(err)
		}
		// A clean mutation: accept path, all reverse importers rechecked.
		res, err := s.SetBody("demo/lib", "Double", "return v * 3")
		if err != nil {
			t.Fatalf("serial=%v: %v", serial, err)
		}
		okJSON, _ := json.Marshal(res["packages_rechecked"])
		// A breaking mutation: reject path with diagnostics.
		_, err = s.SetBody("demo/lib", "Double", "return undefinedIdent(v)")
		rej, ok := err.(*Reject)
		if !ok {
			t.Fatalf("serial=%v: want Reject, got %v", serial, err)
		}
		rejJSON, _ := json.Marshal(rej.Diagnostics)
		// The two runs use two fixture copies; only the tempdir differs.
		return string(okJSON), strings.ReplaceAll(string(rejJSON), s.dir, "FIX")
	}
	sOK, sRej := run(true)
	pOK, pRej := run(false)
	if sOK != pOK {
		t.Fatalf("packages_rechecked diverge: serial=%s parallel=%s", sOK, pOK)
	}
	if sRej != pRej {
		t.Fatalf("diagnostics diverge:\nserial:   %s\nparallel: %s", sRej, pRej)
	}
}
