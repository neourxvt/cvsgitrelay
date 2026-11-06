package main

import "testing"

// Schedulers rely on 75 meaning "run again later" and on every permanent
// failure staying outside it.
func TestExitCodes(t *testing.T) {
	for code, want := range map[string]int{
		"retry": 75, "locked": 75, "timeout": 75,
		"source": 5, "parse": 5, "integrity": 5,
		"conflict": 6, "subprocess": 7, "limit": 7,
		"config": 3, "io": 74, "internal": 70,
	} {
		if got := exitCode(code); got != want {
			t.Errorf("%s: exit %d, want %d", code, got, want)
		}
	}
}
