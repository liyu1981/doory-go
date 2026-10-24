// The command-line surface of the single binary (M13: the upstream management
// commands).
//
// Django's `manage.py <command> [options]` becomes `doory <command> [options]`.
// The observable contract the parity cli-driver compares is: exit code, stdout,
// stderr, database rows and media files. Framework-level differences are
// deliberate and recorded (D13): there is no --settings/--pythonpath/--traceback
// global option block (a single binary has no settings module), and `doory help`
// lists the ported commands instead of Django's app registry.
package doory

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// CLI is one command invocation: the app, the streams and the parsed options.
type CLI struct {
	App    *App
	Stdout io.Writer
	Stderr io.Writer
}

// Printf writes to stdout (Command.stdout.write).
func (c *CLI) Printf(format string, args ...any) { fmt.Fprintf(c.Stdout, format, args...) }

// Println writes a line to stdout.
func (c *CLI) Println(text string) { fmt.Fprintln(c.Stdout, text) }

// Errorf writes to stderr.
func (c *CLI) Errorf(format string, args ...any) { fmt.Fprintf(c.Stderr, format, args...) }

// commandError is BaseCommand's CommandError: the message goes out as
// "CommandError: <msg>" and the process exits 1.
type commandError struct{ message string }

func (e *commandError) Error() string { return e.message }

func commandErrorf(format string, args ...any) error {
	return &commandError{message: fmt.Sprintf(format, args...)}
}

// usageError is argparse's failure: the usage block, then
// "<prog> <command>: error: <msg>", exit 2.
type usageError struct {
	usage   string
	command string
	message string
}

func (e *usageError) Error() string { return e.message }

type cliCommand struct {
	name  string
	usage string // the bracket part of the usage line
	run   func(c *CLI, args []string) error
}

// cliCommands is the registry. Order is the help listing order (alphabetical).
var cliCommands = []cliCommand{
	{name: "collab_maintenance", usage: "collab_maintenance [-h]", run: runCollabMaintenance},
	{name: "import_size_presets", usage: "import_size_presets [-h] [--mode {extend,replace}] [--dry-run] paths [paths ...]", run: runImportSizePresets},
	{name: "purge_exports", usage: "purge_exports [-h]", run: runPurgeExports},
	{name: "purge_trash", usage: "purge_trash [-h] [--days DAYS]", run: runPurgeTrash},
	{name: "reencrypt_ai_secrets", usage: "reencrypt_ai_secrets [-h] [--dry-run]", run: runReencryptAISecrets},
	{name: "seed_gallery", usage: "seed_gallery [-h] [--update]", run: runSeedGallery},
	{name: "seed_templates", usage: "seed_templates [-h] [--update] [--dir DATA_DIR]", run: runSeedTemplates},
	{name: "import_template", usage: "import_template [-h] [--publish] [--create-categories] paths [paths ...]", run: runImportTemplate},
	{name: "seed_size_presets", usage: "seed_size_presets [-h] [--update]", run: runSeedSizePresets},
	{name: "sync_models", usage: "sync_models [-h] [--model ID [ID ...]] [--all] [--list] [--dry-run] [--force] [--from-dir DIR]", run: runSyncModels},
	{name: "import_elements", usage: "import_elements [-h] --pack PACK [--name NAME] [--kind {shape,line,icon,sticker,illustration,frame,graphic}] [--license LICENSE] [--source-url SOURCE_URL] directory", run: runImportElements},
	{name: "seed_elements", usage: "seed_elements [-h]", run: runSeedElements},
	{name: "sync_element_packs", usage: "sync_element_packs [-h] [--pack ID [ID ...]] [--list] [--dry-run] [--force] [--prune] [--brand-logos | --no-brand-logos] [--concurrency N] [--from-dir DIR]", run: runSyncElementPacks},
	{name: "sync_audio", usage: "sync_audio [-h] [--from-dir DIR] [--pack ID [ID ...]] [--all-packs] [--list] [--dry-run] [--force] [--prune]", run: runSyncAudio},
	{name: "sync_fonts", usage: "sync_fonts [-h] [--all | --popular N | --from-dir DIR] [--subsets SUBSETS] [--weights WEIGHTS] [--styles STYLES] [--concurrency N] [--license LICENSE] [families ...]", run: runSyncFonts},
}

// RunCLI dispatches one command line; returns the process exit code.
func RunCLI(app *App, argv []string, stdout, stderr io.Writer) int {
	c := &CLI{App: app, Stdout: stdout, Stderr: stderr}
	if len(argv) == 0 {
		PrintCLIHelp(stdout)
		return 0
	}
	name := argv[0]
	if name == "help" || name == "--help" || name == "-h" {
		PrintCLIHelp(stdout)
		return 0
	}
	for _, cmd := range cliCommands {
		if cmd.name != name {
			continue
		}
		// Django runs the system checks after argparse accepted the
		// command line and before the command body, printing warnings to
		// stderr (base.py check()). Buffer the command's stderr so a usage
		// error (argparse exits first) never gets the banner.
		var stderrBuf bytes.Buffer
		buffered := &CLI{App: c.App, Stdout: c.Stdout, Stderr: &stderrBuf}
		err := cmd.run(buffered, argv[1:])
		if _, usage := err.(*usageError); !usage {
			c.App.printSystemChecks(stderr)
			_, _ = stderr.Write(stderrBuf.Bytes())
		} else {
			_, _ = stderr.Write(stderrBuf.Bytes())
		}
		if err == nil {
			return 0
		}
		var ue *usageError
		if asUsageError(err, &ue) {
			ue.command = name
			if ue.usage == "" {
				ue.usage = cmd.usage
			}
			fmt.Fprintf(stderr, "usage: doory %s\n", ue.usage)
			fmt.Fprintf(stderr, "doory %s: error: %s\n", name, ue.message)
			return 2
		}
		var ce *commandError
		if asCommandError(err, &ce) {
			fmt.Fprintf(stderr, "CommandError: %s\n", ce.message)
			return 1
		}
		fmt.Fprintf(stderr, "CommandError: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stderr, "Unknown command: '%s'\nType 'doory help' for usage.\n", name)
	return 1
}

func asUsageError(err error, target **usageError) bool {
	ue, ok := err.(*usageError)
	if ok {
		*target = ue
	}
	return ok
}

func asCommandError(err error, target **commandError) bool {
	ce, ok := err.(*commandError)
	if ok {
		*target = ce
	}
	return ok
}

// PrintCLIHelp writes the command overview: the built-in subcommands and the
// ported management commands. `doory` with no arguments and `doory help` both
// print this (Django's manage.py does the same).
func PrintCLIHelp(w io.Writer) {
	fmt.Fprintln(w, "usage: doory <command> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Built-in commands:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    serve         HTTP + WebSockets + MCP + the embedded frontend (default port :8000)")
	fmt.Fprintln(w, "    entrypoint    wait for the database, migrate, optional content bootstraps, then serve")
	fmt.Fprintln(w, "    migrate       apply the embedded schema for the DATABASE_URL dialect")
	fmt.Fprintln(w, "    healthcheck   probe /api/v1/health/ (HEALTH_PORT, HEALTH_PATH); exit 0 on 200")
	fmt.Fprintln(w, "    version       print the port version and its upstream pin")
	fmt.Fprintln(w, "    help          this text (also `doory help <command>`)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Management commands (Django's manage.py equivalents):")
	fmt.Fprintln(w)
	names := make([]string, 0, len(cliCommands))
	for _, cmd := range cliCommands {
		names = append(names, cmd.name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintln(w, "    "+name)
	}
}

// RunCLIFromEnv is the cmd/doory entry point: it opens the database from the
// environment and runs the command.
func RunCLIFromEnv(argv []string) int {
	s := FromEnv()
	db, _, err := OpenDB(s.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CommandError: could not open the database:", err)
		return 1
	}
	defer db.Close()
	return RunCLI(NewApp(s, db), argv, os.Stdout, os.Stderr)
}

var _ = strings.TrimSpace

// HasCommand reports whether a management command is ported (the entrypoint
// guards each bootstrap step with it, like `manage.py help <cmd>` upstream).
func HasCommand(name string) bool {
	for _, cmd := range cliCommands {
		if cmd.name == name {
			return true
		}
	}
	return false
}
