// Transactional email (apps/accounts/emails.py): multipart (plain text plus
// the branded HTML shell), sent off the request thread unless
// EMAIL_SEND_ASYNC is off.
//
// The transport follows EMAIL_URL like django-environ's env.email()
// (consolemail:// — the upstream default — prints to stdout, smtp/smtps speak
// SMTP). D9: template rendering is Go's; parity asserts links/tokens and text
// structure, not HTML bytes.
package doory

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// emailConfig is what EMAIL_URL configures (env.email()).
type emailConfig struct {
	Backend string // "console" | "smtp"
	Host    string
	Port    int
	User    string
	Password string
	UseTLS  bool
	UseSSL  bool
	From    string
}

// parseEmailURL ports django-environ's env.email for the schemes the port
// supports (consolemail/dummy/filebased plus smtp/smtps).
func parseEmailURL(raw string) emailConfig {
	cfg := emailConfig{Backend: "console", Host: "localhost", Port: 25, From: "webmaster@localhost"}
	if strings.TrimSpace(raw) == "" {
		raw = "consolemail://"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return cfg
	}
	switch strings.ToLower(parsed.Scheme) {
	case "smtp", "smtps":
		cfg.Backend = "smtp"
		if parsed.Hostname() != "" {
			cfg.Host = parsed.Hostname()
		}
		if parsed.Port() != "" {
			if port, err := strconv.Atoi(parsed.Port()); err == nil {
				cfg.Port = port
			}
		} else if strings.ToLower(parsed.Scheme) == "smtps" {
			cfg.Port = 465
		}
		if parsed.User != nil {
			cfg.User = parsed.User.Username()
			cfg.Password, _ = parsed.User.Password()
		}
		query := parsed.Query()
		cfg.UseTLS = strings.EqualFold(query.Get("tls"), "true")
		cfg.UseSSL = strings.EqualFold(query.Get("ssl"), "true") || strings.ToLower(parsed.Scheme) == "smtps"
		if from := query.Get("from"); from != "" {
			cfg.From = from
		}
	default:
		// consolemail://, dummy://, filebased://, locmem://: nothing is
		// delivered; the console backend prints.
		cfg.Backend = "console"
	}
	if cfg.From == "" {
		cfg.From = "webmaster@localhost"
	}
	return cfg
}

// sendAccountEmail ports emails._send: renders the two bodies, then hands the
// message to the configured transport (off the request thread when async).
func (a *App) sendAccountEmail(subject, to, text, html string) {
	cfg := a.Settings.emailConfig()
	deliver := func() {
		switch cfg.Backend {
		case "smtp":
			if err := smtpSend(cfg, to, subject, text, html); err != nil {
				logerr(err, "sendAccountEmail.smtp")
			}
		default:
			// Django's console backend writes the message to stdout.
			fmt.Fprintf(os.Stdout, "Content-Type: text/plain; charset=\"utf-8\"\nMIME-Version: 1.0\n"+
				"Content-Transfer-Encoding: 7bit\nSubject: %s\nFrom: %s\nTo: %s\nDate: %s\n\n%s\n",
				subject, cfg.From, to, time.Now().Format(time.RFC1123Z), text)
		}
	}
	if a.Settings.EmailSendAsync {
		go deliver()
		return
	}
	deliver()
}

// smtpSend delivers one multipart/alternative message over SMTP.
func smtpSend(cfg emailConfig, to, subject, text, html string) error {
	boundary := "doory-" + newUUID4Hex()
	body := "--" + boundary + "\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n\r\n" + text + "\r\n" +
		"--" + boundary + "\r\nContent-Type: text/html; charset=\"utf-8\"\r\n\r\n" + html + "\r\n" +
		"--" + boundary + "--\r\n"
	message := "From: " + cfg.From + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"" + boundary +
		"\"\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + body

	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var auth smtp.Auth
	if cfg.User != "" {
		auth = smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)
	}
	if cfg.UseSSL {
		conn, err := tls.Dial("tcp", address, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		client, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			return err
		}
		defer client.Close()
		if auth != nil {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
		if err := client.Mail(cfg.From); err != nil {
			return err
		}
		if err := client.Rcpt(to); err != nil {
			return err
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(message)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return client.Quit()
	}
	client, err := smtp.Dial(address)
	if err != nil {
		return err
	}
	defer client.Close()
	if cfg.UseTLS {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(message)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// emailLink ports emails._link: SITE_URL + the in-app path + urlencoded
// uid/token (Python's urlencode order: uid, token).
func (a *App) emailLink(path string, u *UserRow, token string) string {
	return strings.TrimRight(a.Settings.SiteURL, "/") + path +
		"?uid=" + url.QueryEscape(encodeUID(u)) + "&token=" + url.QueryEscape(token)
}

// emailHTMLShell wraps a body in the branded HTML shell (base.html's
// structure, D9).
func emailHTMLShell(title, content string) string {
	return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <meta name="color-scheme" content="light">
    <title>` + title + `</title>
  </head>
  <body style="margin:0;padding:0;background-color:#f9f7ff;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background-color:#f9f7ff;">
      <tr>
        <td align="center" style="padding:32px 16px;">
          <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background-color:#ffffff;border:1px solid #e6e0f2;border-radius:12px;">
            <tr>
              <td style="padding:28px 32px 0 32px;font-family:Georgia,'Times New Roman',serif;font-size:24px;font-style:italic;font-weight:700;color:#5b43c4;letter-spacing:-0.01em;">
                Doory
              </td>
            </tr>
            <tr>
              <td style="padding:16px 32px 32px 32px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:16px;line-height:1.55;color:#2b1c17;">
                ` + content + `
              </td>
            </tr>
          </table>
          <p style="max-width:520px;margin:16px 0 0 0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:12px;line-height:1.5;color:#6f6170;text-align:center;">
            You are receiving this because someone used your email address on Doory. If that was not you, you can ignore this message.
          </p>
        </td>
      </tr>
    </table>
  </body>
</html>`
}

// userDisplayName is User.name (display_name or the email's local part).
func userDisplayName(u *UserRow) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if at := strings.IndexByte(u.Email, '@'); at > 0 {
		return u.Email[:at]
	}
	return u.Email
}

// sendVerificationEmail ports emails.send_verification_email.
func (a *App) sendVerificationEmail(u *UserRow) {
	link := a.emailLink("/verify-email", u, a.Settings.makeEmailVerificationToken(u))
	name := userDisplayName(u)
	text := "Hi " + name + ",\n\n" +
		"Welcome to Doory. Confirm your email address by opening this link:\n\n" +
		link + "\n\n" +
		"The link expires in 24 hours. If you did not create a Doory account, you can ignore this message.\n"
	html := emailHTMLShell("Verify your email address for Doory",
		"<p>Hi "+htmlEscape(name)+",</p><p>Welcome to Doory. Confirm your email address by opening this link:</p>"+
			`<p><a href="`+htmlEscape(link)+`" style="color:#5b43c4;">Confirm my email address</a></p>`+
			"<p>The link expires in 24 hours. If you did not create a Doory account, you can ignore this message.</p>")
	a.sendAccountEmail("Verify your email address for Doory", u.Email, text, html)
}

// sendPasswordResetEmail ports emails.send_password_reset_email.
func (a *App) sendPasswordResetEmail(u *UserRow) {
	link := a.emailLink("/reset-password", u, a.Settings.makePasswordResetToken(u))
	name := userDisplayName(u)
	text := "Hi " + name + ",\n\n" +
		"We received a request to set a new password for your Doory account. Choose one by opening this link:\n\n" +
		link + "\n\n" +
		"The link expires in 24 hours and works once. If you did not ask for this, ignore this message: your password stays the same.\n"
	html := emailHTMLShell("Reset your Doory password",
		"<p>Hi "+htmlEscape(name)+",</p><p>We received a request to set a new password for your Doory account. Choose one by opening this link:</p>"+
			`<p><a href="`+htmlEscape(link)+`" style="color:#5b43c4;">Choose a new password</a></p>`+
			"<p>The link expires in 24 hours and works once. If you did not ask for this, ignore this message: your password stays the same.</p>")
	a.sendAccountEmail("Reset your Doory password", u.Email, text, html)
}

// htmlEscape escapes the characters that matter inside the HTML bodies.
func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}
