package main

import "testing"

// The agent tells an older client to update. Getting the comparison wrong
// nags every client forever or never nags one that needs it.

func TestVersionOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.2.0", "0.1.0", false},
		{"0.1.0", "0.1.0", false},
		{"0.9.0", "0.10.0", true},
		{"1.0.0", "0.99.99", false},
		{"0.1.9", "0.1.10", true},
		{"0.1", "0.1.1", true},
		{"1", "1.0.0", false},
	}
	for _, c := range cases {
		if got := isVersionOlder(c.a, c.b); got != c.want {
			t.Errorf("isVersionOlder(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestGarbageVersionsDoNotPanic(t *testing.T) {
	for _, c := range [][2]string{{"", "1.0.0"}, {"1.0.0", ""}, {"", ""}, {"x.y.z", "1.0.0"}, {"1.0.0", "x.y.z"}} {
		_ = isVersionOlder(c[0], c[1])
	}
}
