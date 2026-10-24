// A small argparse-compatible option parser for the management commands:
// `--name value`, `--name=value`, boolean `--flag`, `--` terminator and
// positionals. The error messages are argparse's, because the cli-driver
// compares stderr (normalizing only the Django global-option block, D13).

package doory

import (
	"fmt"
	"strconv"
	"strings"
)

// argSpec is one declared option.
type argSpec struct {
	name    string // without the leading --
	kind    string // "bool" | "string" | "int" | "list"
	choices []string
	// dest is the value key; defaults to name with - replaced by _.
	dest string
}

// parsedArgs holds the parsed values and positionals.
type parsedArgs struct {
	values      map[string]any
	positionals []string
}

// bool returns a boolean option.
func (p *parsedArgs) bool(name string) bool {
	v, _ := p.values[name].(bool)
	return v
}

// str returns a string option ("" when absent).
func (p *parsedArgs) str(name string) string {
	v, _ := p.values[name].(string)
	return v
}

// has reports whether the option was given.
func (p *parsedArgs) has(name string) bool {
	_, ok := p.values[name]
	return ok
}

// intOption returns an int with a default.
func (p *parsedArgs) intOption(name string, fallback int) int {
	if v, ok := p.values[name].(int); ok {
		return v
	}
	return fallback
}

// list returns a list option in the order given.
func (p *parsedArgs) list(name string) []string {
	v, _ := p.values[name].([]string)
	return v
}

// parseArgs parses argv against specs. Boolean options never consume the next
// argument; every other option requires one.
func parseArgs(argv []string, specs []argSpec) (*parsedArgs, error) {
	byName := map[string]argSpec{}
	for _, s := range specs {
		byName[s.name] = s
	}
	out := &parsedArgs{values: map[string]any{}}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			out.positionals = append(out.positionals, argv[i+1:]...)
			break
		}
		if arg == "-h" || arg == "--help" {
			return nil, &usageError{message: "help requested", usage: "help"}
		}
		if strings.HasPrefix(arg, "--") {
			name, value, hasValue := strings.Cut(arg[2:], "=")
			spec, ok := byName[name]
			if !ok {
				return nil, &usageError{message: "unrecognized arguments: " + arg}
			}
			dest := spec.dest
			if dest == "" {
				dest = strings.ReplaceAll(spec.name, "-", "_")
			}
			switch spec.kind {
			case "bool":
				if hasValue {
					parsed, err := parseBool(value)
					if err != nil {
						return nil, &usageError{message: fmt.Sprintf("argument --%s: ignored explicit argument %q", name, value)}
					}
					out.values[dest] = parsed
					continue
				}
				out.values[dest] = true
			case "list":
				// nargs="+" with action="extend": every following non-option
				// token belongs to this list until the next option.
				items, _ := out.values[dest].([]string)
				if hasValue {
					items = append(items, value)
					out.values[dest] = items
					continue
				}
				collected := []string{}
				for i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					i++
					collected = append(collected, argv[i])
				}
				if len(collected) == 0 {
					return nil, &usageError{message: fmt.Sprintf("argument --%s: expected at least one argument", name)}
				}
				out.values[dest] = append(items, collected...)
			default: // string | int
				if !hasValue {
					if i+1 >= len(argv) {
						return nil, &usageError{message: fmt.Sprintf("argument --%s: expected one argument", name)}
					}
					i++
					value = argv[i]
				}
				if spec.kind == "int" {
					n, err := strconv.Atoi(value)
					if err != nil {
						return nil, &usageError{message: fmt.Sprintf("argument --%s: invalid int value: '%s'", name, value)}
					}
					out.values[dest] = n
					continue
				}
				if len(spec.choices) > 0 && !containsString(spec.choices, value) {
					return nil, &usageError{message: fmt.Sprintf("argument --%s: invalid choice: '%s' (choose from %s)",
						name, value, quoteChoices(spec.choices))}
				}
				out.values[dest] = value
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			return nil, &usageError{message: "unrecognized arguments: " + arg}
		}
		out.positionals = append(out.positionals, arg)
	}
	return out, nil
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %s", value)
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// quoteChoices matches this Python line's argparse: choices are listed
// unquoted ("choose from extend, replace").
func quoteChoices(values []string) string {
	return strings.Join(values, ", ")
}
