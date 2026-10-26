// Who may join which room, and the handshake origin rule
// (apps/collab/access.py). Synchronous; the connection flow calls these
// before admitting a member.

// Ports backend/apps/collab/access.py
package doory

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CollabGrant is access.Grant: what a connection was admitted with.
type CollabGrant struct {
	Role     string // owner | edit | comment | view
	LinkID   string // "" for the owner
	Title    string
	Revision int64
}

// Sentinel errors matching access.py's exception types (the consumer maps
// them to close codes: Forbidden -> 4403, DesignGone -> 4404).
var (
	errCollabForbidden    = errors.New("collab: forbidden")
	errCollabDesignGone   = errors.New("collab: design_trashed")
	errCollabPasswordNeed = errors.New("collab: password_needed")
)

// CollabLocked is access.Locked (link password window exhausted).
type CollabLocked struct{ RetryAfter float64 }

func (e *CollabLocked) Error() string { return "locked" }

// ownerGrant ports access.owner_grant: the owner's grant; nil grant (no
// error) when the user does not own the design (or it does not exist);
// DesignGone when it is trashed.
func (a *App) ownerGrant(u *UserRow, designID string) (*CollabGrant, error) {
	// designID is the canonical dashed uuid (or its storage hex); the tables
	// store uuid.hex (32 hex) - hexUUID strips the dashes.
	designID = hexUUID(strings.ToLower(designID))
	var title string
	var revision int64
	var trashed any
	err := a.DB.QueryRow(
		`SELECT title, revision, trashed_at FROM designs_design WHERE id = ? AND owner_id = ?`,
		designID, u.ID).Scan(&title, &revision, &trashed)
	if err != nil {
		return nil, nil
	}
	if trashed != nil {
		return nil, errCollabDesignGone
	}
	return &CollabGrant{Role: "owner", Title: title, Revision: revision}, nil
}

// linkGrant ports access.link_grant: the grant of a share link for this
// design; Forbidden / password-needed / Locked map 1:1 to the exceptions.
func (a *App) linkGrant(designID, token, password, clientIP string) (CollabGrant, error) {
	link, design, ok := a.resolveLink(token)
	if !ok {
		return CollabGrant{}, errCollabForbidden
	}
	if !strings.EqualFold(link.DesignID, designID) {
		return CollabGrant{}, errCollabForbidden
	}
	if err := a.enforceLinkPassword(link, password, clientIP); err != nil {
		var locked *passwordLockedError
		if errors.As(err, &locked) {
			return CollabGrant{}, &CollabLocked{RetryAfter: locked.retryAfter}
		}
		return CollabGrant{}, errCollabPasswordNeed
	}
	// record_access: one UPDATE, never part of any response (D2 note).
	_, _ = a.DB.Exec(
		`UPDATE sharing_sharelink SET access_count = access_count + 1, last_accessed_at = ?
		 WHERE id = ?`, formatDjangoDateTime(time.Now()), link.ID)
	return CollabGrant{
		Role: link.Role, LinkID: link.ID, Title: design.Title, Revision: design.Revision,
	}, nil
}

// ------------------------------------------------------------- handshake ----

// OriginAllowed ports access.origin_allowed: the page that opens the socket
// must be this site — a trusted origin (CSRF settings + SITE_URL) or the
// same host (validated against DJANGO_ALLOWED_HOSTS).
func (s *Settings) OriginAllowed(origin, host string) bool {
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if parsed.Hostname() == "" {
		return false
	}
	normalised := strings.ToLower(originOf(origin))
	for _, pattern := range append(append([]string{}, s.CSRFTrustedOrigins...), s.SiteURL) {
		if pattern == "" {
			continue
		}
		if globMatch(normalised, strings.ToLower(originOf(pattern))) {
			return true
		}
	}
	if host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host) &&
		s.validateHost(strings.ToLower(parsed.Hostname()))
}

// globMatch ports fnmatchcase for origin patterns (fnmatch syntax: * ? []).
func globMatch(name, pattern string) bool {
	if pattern == "" {
		return false
	}
	var re strings.Builder
	re.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		case '[':
			re.WriteString("[")
		case ']':
			re.WriteString("]")
		default:
			re.WriteString(regexpQuote(string(r)))
		}
	}
	re.WriteString("$")
	ok, err := regexp.MatchString(re.String(), name)
	return err == nil && ok
}

func regexpQuote(s string) string {
	return regexp.MustCompile(`\\.+`).ReplaceAllString(s, `\$0`)
}

// validateHost ports Django's validate_host + is_same_domain: exact match, or
// a leading-dot pattern matching the domain and all subdomains; "*" matches
// anything. The host is lowercased with no port.
func (s *Settings) validateHost(host string) bool {
	for _, pattern := range s.AllowedHosts {
		if pattern == "*" {
			return true
		}
		pattern = strings.ToLower(pattern)
		if pattern == "" {
			return false
		}
		if pattern[0] == '.' {
			if strings.HasSuffix(host, pattern) || host == pattern[1:] {
				return true
			}
		} else if pattern == host {
			return true
		}
	}
	return false
}
