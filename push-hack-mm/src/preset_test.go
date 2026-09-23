package main

import "testing"

func TestPresetNameRejectsPathEscapes(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", "a.pumma", "..", `a\b`, "x\ny"} {
		if presetName.MatchString(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, ok := range []string{"Kit 1", "bass_01", "a-b"} {
		if !presetName.MatchString(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
}
