// The built-in `doory config` command: generate and upgrade the optional .env
// file. It never opens the database, so it can be run before the first start.
package doory

import (
	"fmt"
	"io"
	"strings"

	"doory-go/internal/envconfig"
)

// RunConfigCommand dispatches `doory config <subcommand>`; returns the process
// exit code.
func RunConfigCommand(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 || argv[0] == "help" || argv[0] == "-h" || argv[0] == "--help" {
		printConfigHelp(stdout)
		return 0
	}
	switch argv[0] {
	case "init":
		return runConfigInit(argv[1:], stdout, stderr)
	case "upgrade":
		return runConfigUpgrade(argv[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Unknown config command: '%s'\n\n", argv[0])
		printConfigHelp(stderr)
		return 2
	}
}

func printConfigHelp(w io.Writer) {
	fmt.Fprintln(w, "usage: doory config <command> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    init [--force] [DIR]   write DIR/.env (default: ./.env) from the embedded .env.example")
	fmt.Fprintln(w, "    upgrade [FILE]         merge the latest template into FILE (default: ./.env),")
	fmt.Fprintln(w, "                           keeping every value you have configured")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The binary reads ~/.config/doory/.env when present, or a file passed with")
	fmt.Fprintln(w, "the global --env-file option. Process environment values always win.")
}

func runConfigInit(argv []string, stdout, stderr io.Writer) int {
	args, err := parseArgs(argv, []argSpec{{name: "force", kind: "bool"}})
	if err != nil {
		return configUsageError("init", "init [--force] [DIR]", err, stderr)
	}
	if len(args.positionals) > 1 {
		return configUsageError("init", "init [--force] [DIR]",
			&usageError{message: "unrecognized arguments: " + strings.Join(args.positionals[1:], " ")}, stderr)
	}
	dir := "."
	if len(args.positionals) == 1 {
		dir = args.positionals[0]
	}
	path, err := envconfig.Init(dir, args.bool("force"))
	if err != nil {
		fmt.Fprintf(stderr, "doory config init: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s (template v%d)\n", path, envconfig.TemplateVersion())
	if !args.bool("force") {
		rel := path
		if strings.HasPrefix(rel, "./") {
			rel = strings.TrimPrefix(rel, "./")
		}
		fmt.Fprintf(stdout, "use it with 'doory --env-file %s <command>', or move it to ~/.config/doory/.env to load it by default.\n", rel)
	}
	return 0
}

func runConfigUpgrade(argv []string, stdout, stderr io.Writer) int {
	args, err := parseArgs(argv, nil)
	if err != nil {
		return configUsageError("upgrade", "upgrade [FILE]", err, stderr)
	}
	if len(args.positionals) > 1 {
		return configUsageError("upgrade", "upgrade [FILE]",
			&usageError{message: "unrecognized arguments: " + strings.Join(args.positionals[1:], " ")}, stderr)
	}
	path := ".env"
	if len(args.positionals) == 1 {
		path = args.positionals[0]
	}
	result, err := envconfig.Upgrade(path)
	if err != nil {
		fmt.Fprintf(stderr, "doory config upgrade: %v\n", err)
		return 1
	}
	if !result.Changed {
		fmt.Fprintf(stdout, "%s is already up to date (template v%d).\n", result.Path, result.NewVersion)
		return 0
	}
	fmt.Fprintf(stdout, "upgraded %s: template %s -> v%d\n",
		result.Path, versionLabel(result.OldVersion), result.NewVersion)
	if len(result.Added) > 0 {
		fmt.Fprintf(stdout, "  added: %s\n", strings.Join(result.Added, ", "))
	}
	if len(result.Documented) > 0 {
		fmt.Fprintf(stdout, "  documented %d new setting(s) as comments\n", len(result.Documented))
	}
	if len(result.Extra) > 0 {
		fmt.Fprintf(stdout, "  kept settings not in the template: %s\n", strings.Join(result.Extra, ", "))
	}
	fmt.Fprintf(stdout, "  kept %d configured value(s); backup: %s\n", len(result.Preserved), result.BackupPath)
	return 0
}

func versionLabel(version int) string {
	if version == 0 {
		return "(no version)"
	}
	return fmt.Sprintf("v%d", version)
}

func configUsageError(command, usage string, err error, stderr io.Writer) int {
	message := err.Error()
	if usageErr, ok := err.(*usageError); ok {
		message = usageErr.message
	}
	fmt.Fprintf(stderr, "usage: doory config %s\n", usage)
	fmt.Fprintf(stderr, "doory config %s: error: %s\n", command, message)
	return 2
}
