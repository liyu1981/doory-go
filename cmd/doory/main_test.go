// Command-line splitting: a bare binary prints the usage text (no panic, no
// implicit serve), and the first argument is the subcommand.
package main

import (
	"reflect"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		argv     []string
		wantCmd  string
		wantArgs []string
	}{
		{nil, "", nil},
		{[]string{}, "", nil},
		{[]string{"serve"}, "serve", []string{}},
		{[]string{"serve", "127.0.0.1:8080"}, "serve", []string{"127.0.0.1:8080"}},
		{[]string{"migrate"}, "migrate", []string{}},
		{[]string{"purge_trash", "--days", "0"}, "purge_trash", []string{"--days", "0"}},
		{[]string{"entrypoint", "--prepare"}, "entrypoint", []string{"--prepare"}},
	}
	for _, tc := range cases {
		cmd, args := splitArgs(tc.argv)
		if cmd != tc.wantCmd {
			t.Errorf("splitArgs(%v) cmd = %q, want %q", tc.argv, cmd, tc.wantCmd)
		}
		if !reflect.DeepEqual(args, tc.wantArgs) {
			t.Errorf("splitArgs(%v) args = %v, want %v", tc.argv, args, tc.wantArgs)
		}
	}
}
