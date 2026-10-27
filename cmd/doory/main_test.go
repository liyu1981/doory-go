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

func TestTakeGlobalFlags(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantRest []string
		wantOpts globalOptions
		wantErr  bool
	}{
		{"no flags", []string{"serve", ":8111"}, []string{"serve", ":8111"}, globalOptions{}, false},
		{"before command", []string{"--env-file", "/tmp/x.env", "serve"}, []string{"serve"}, globalOptions{envFile: "/tmp/x.env"}, false},
		{"after command", []string{"serve", "--env-file=/tmp/x.env"}, []string{"serve"}, globalOptions{envFile: "/tmp/x.env"}, false},
		{"disable", []string{"--no-env-file", "migrate"}, []string{"migrate"}, globalOptions{noEnvFile: true}, false},
		{"missing path", []string{"--env-file"}, nil, globalOptions{}, true},
		{"empty path", []string{"--env-file=", "serve"}, nil, globalOptions{}, true},
		{"conflict", []string{"--no-env-file", "--env-file", "/tmp/x", "serve"}, nil, globalOptions{}, true},
		{"after terminator", []string{"serve", "--", "--env-file"}, []string{"serve", "--", "--env-file"}, globalOptions{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, opts, err := takeGlobalFlags(tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
			if opts != tc.wantOpts {
				t.Errorf("opts = %+v, want %+v", opts, tc.wantOpts)
			}
		})
	}
}
