// The bootstrap commands self-host installs need: createsuperuser (Django's
// built-in, the login field is the email here), changepassword and make-admin
// (promote an existing account).
package doory

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"
)

var adminEmailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func runCreateSuperuser(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "email", kind: "string"},
		{name: "noinput", kind: "bool", dest: "noinput"},
		{name: "no-input", kind: "bool", dest: "noinput"},
		{name: "database", kind: "string", choices: []string{"default"}},
	})
	if err != nil {
		return err
	}
	if len(args.positionals) > 0 {
		return &usageError{message: "unrecognized arguments: " + strings.Join(args.positionals, " ")}
	}
	interactive := !args.bool("noinput")
	email := strings.TrimSpace(args.str("email"))
	password := os.Getenv("DJANGO_SUPERUSER_PASSWORD")
	if email == "" {
		email = strings.TrimSpace(os.Getenv("DJANGO_SUPERUSER_EMAIL"))
	}
	if !interactive {
		if email == "" {
			return commandErrorf("You must use --email with --noinput.")
		}
	} else {
		if !stdinIsTerminal() {
			c.Println("Superuser creation skipped due to not running in a TTY. " +
				"You can run `doory createsuperuser` in your project to create one manually.")
			return nil
		}
		reader := bufio.NewReader(os.Stdin)
		for {
			if email == "" {
				c.Printf("Email: ")
				line, _ := reader.ReadString('\n')
				email = strings.TrimSpace(line)
			}
			if email != "" && adminEmailPattern.MatchString(email) {
				break
			}
			c.Errorf("Error: Enter a valid email address.\n")
			email = ""
		}
		for {
			first := readPasswordLine(reader, "Password: ")
			if strings.TrimSpace(first) == "" {
				c.Errorf("Error: Blank passwords aren't allowed.\n")
				continue
			}
			second := readPasswordLine(reader, "Password (again): ")
			if first != second {
				c.Errorf("Error: Your passwords didn't match.\n")
				continue
			}
			problems := validateNewPassword(first, email)
			if len(problems) > 0 {
				var lines []string
				for _, problem := range problems {
					lines = append(lines, problem.Message)
				}
				c.Errorf("%s\n", strings.Join(lines, "\n"))
				c.Printf("Bypass password validation and create user anyway? [y/N]: ")
				line, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					continue
				}
			}
			password = first
			break
		}
	}
	if email == "" || !adminEmailPattern.MatchString(email) {
		return commandErrorf("Enter a valid email address.")
	}
	var existing int
	_ = c.App.DB.QueryRow(`SELECT COUNT(*) FROM accounts_user WHERE lower(email) = lower(?)`,
		email).Scan(&existing)
	if existing > 0 {
		return commandErrorf("Error: That email is already taken.")
	}
	encoded := "!"
	if password != "" {
		encoded = hashPassword(password)
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	if _, err := c.App.DB.Exec(`INSERT INTO accounts_user
		(password, last_login, is_superuser, id, created_at, updated_at, email, display_name,
		 avatar, google_sub, is_email_verified, is_active, is_staff)
		VALUES (?, NULL, TRUE, ?, ?, ?, ?, '', '', NULL, TRUE, TRUE, TRUE)`,
		encoded, id, now, now, strings.ToLower(email)); err != nil {
		return commandErrorf("%v", err)
	}
	c.Println("Superuser created successfully.")
	return nil
}

func runChangePassword(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "database", kind: "string", choices: []string{"default"}},
	})
	if err != nil {
		return err
	}
	if len(args.positionals) > 1 {
		return &usageError{message: "unrecognized arguments: " + strings.Join(args.positionals[1:], " ")}
	}
	email := ""
	if len(args.positionals) == 1 {
		email = args.positionals[0]
	} else if current, uerr := user.Current(); uerr == nil {
		email = current.Username
	}
	var id, storedEmail, storedHash string
	err = c.App.DB.QueryRow(`SELECT id, email, password FROM accounts_user WHERE email = ?`,
		email).Scan(&id, &storedEmail, &storedHash)
	if err != nil {
		return commandErrorf("user '%s' does not exist", email)
	}
	c.Printf("Changing password for user '%s'\n", storedEmail)
	reader := bufio.NewReader(os.Stdin)
	p1, p2 := "", ""
	validated := false
	count := 0
	for (p1 != p2 || !validated) && count < 3 {
		p1 = readPasswordLine(reader, "Password: ")
		if p1 == "" {
			return commandErrorf("aborted")
		}
		p2 = readPasswordLine(reader, "Password (again): ")
		if p1 != p2 {
			c.Println("Passwords do not match. Please try again.")
			count++
			continue
		}
		problems := validateNewPassword(p2, storedEmail)
		if len(problems) > 0 {
			var lines []string
			for _, problem := range problems {
				lines = append(lines, problem.Message)
			}
			c.Errorf("%s\n", strings.Join(lines, "\n"))
			count++
			continue
		}
		validated = true
	}
	if count == 3 {
		return commandErrorf("Aborting password change for user '%s' after %s attempts",
			storedEmail, "3")
	}
	if _, err := c.App.DB.Exec(`UPDATE accounts_user SET password = ?, updated_at = ? WHERE id = ?`,
		hashPassword(p1), formatDjangoDateTime(time.Now()), id); err != nil {
		return commandErrorf("%v", err)
	}
	return nil
}

func runMakeAdmin(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "superuser", kind: "bool"},
	})
	if err != nil {
		return err
	}
	if len(args.positionals) != 1 {
		return &usageError{message: "the following arguments are required: email"}
	}
	email := strings.ToLower(strings.TrimSpace(args.positionals[0]))
	var id string
	if err := c.App.DB.QueryRow(`SELECT id FROM accounts_user WHERE LOWER(email) = LOWER(?)`, email).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return commandErrorf("user '%s' does not exist", email)
		}
		return commandErrorf("look up '%s': %v", email, err)
	}
	if args.bool("superuser") {
		_, err = c.App.DB.Exec(`UPDATE accounts_user SET is_staff = TRUE, is_superuser = TRUE,
			is_active = TRUE, updated_at = ? WHERE id = ?`, formatDjangoDateTime(time.Now()), id)
	} else {
		_, err = c.App.DB.Exec(`UPDATE accounts_user SET is_staff = TRUE, is_active = TRUE,
			updated_at = ? WHERE id = ?`, formatDjangoDateTime(time.Now()), id)
	}
	if err != nil {
		return commandErrorf("%v", err)
	}
	c.Printf("%s is now an admin.\n", email)
	return nil
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// readPasswordLine reads a hidden password on a terminal and a plain line
// otherwise (a pipe, as in tests and Docker env bootstrap).
func readPasswordLine(reader *bufio.Reader, prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		data, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return ""
		}
		return string(data)
	}
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}
