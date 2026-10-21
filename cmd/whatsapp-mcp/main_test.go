package main

import "testing"

func TestFlagOrEnv(t *testing.T) {
	cases := []struct {
		name      string
		flagSet   bool
		flagValue bool
		env       string
		want      bool
	}{
		{"default off", false, false, "", false},
		{"env on", false, false, "1", true},
		{"env other value", false, false, "true", false},
		{"flag on", true, true, "", true},
		{"flag on beats env off", true, true, "0", true},
		{"explicit flag off beats env on", true, false, "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flagOrEnv(tc.flagSet, tc.flagValue, tc.env); got != tc.want {
				t.Fatalf("flagOrEnv(%v, %v, %q) = %v, want %v", tc.flagSet, tc.flagValue, tc.env, got, tc.want)
			}
		})
	}
}
