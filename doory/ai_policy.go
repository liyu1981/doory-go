package doory

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const aiMaxURLLength = 2048

// aiUnsafeURLChars ports apps/assets/providers._UNSAFE_URL_CHARS
// (control/space, DEL..U+FFFF, backslash).
var aiUnsafeURLChars = regexp.MustCompile("[\x00-\x20\x7f-\uffff\\\\]")

type aiPolicy struct {
	name            string
	allowHTTP       bool
	privateHosts    map[string]bool
	privateLiterals bool
	allowedPorts    map[int]bool
}

func (s *Settings) aiOperatorPolicy() aiPolicy {
	hosts := map[string]bool{}
	for _, h := range s.AIAllowedPrivateHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimSuffix(h, ".")
		if h != "" {
			hosts[h] = true
		}
	}
	return aiPolicy{name: "operator", allowHTTP: true, privateHosts: hosts, privateLiterals: true}
}

func (s *Settings) aiUserPolicy() aiPolicy {
	ports := map[int]bool{443: true, 8443: true}
	if s.AIUserProvidersAllowHTTP {
		ports[80] = true
		ports[8080] = true
	}
	for _, p := range s.AIUserProvidersAllowedPorts {
		ports[p] = true
	}
	return aiPolicy{name: "user", allowHTTP: s.AIUserProvidersAllowHTTP, allowedPorts: ports}
}

// aiTransportError is transport.TransportError (code from errors.STATUS).
type aiTransportError struct {
	code    string
	message string
}

func (e *aiTransportError) Error() string { return e.message }

func aiRefused() *aiTransportError {
	return &aiTransportError{code: "refused_address", message: "That server address is not allowed."}
}

// aiEndpoint is transport.Endpoint.
type aiEndpoint struct {
	scheme    string
	host      string
	port      int
	addresses []string
	path      string
}

// aiCheckURL ports transport.check_url. resolve=false checks only the form
// and IP literals (the admin validation path); resolve=true additionally
// resolves host names and checks every address against the policy.
func (s *Settings) aiCheckURL(raw string, policy aiPolicy, resolve bool) (aiEndpoint, *aiTransportError) {
	if len(raw) > aiMaxURLLength || aiUnsafeURLChars.MatchString(raw) {
		return aiEndpoint{}, &aiTransportError{code: "refused_address",
			message: "The server address is too long or contains unsafe characters."}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return aiEndpoint{}, &aiTransportError{code: "refused_address",
			message: "The server address is not a valid URL."}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(policy.allowHTTP && scheme == "http") {
		start := "https://"
		if policy.allowHTTP {
			start = "http:// or https://"
		}
		return aiEndpoint{}, &aiTransportError{code: "refused_address",
			message: "The server address must start with " + start + "."}
	}
	portStr := u.Port()
	effective := 0
	if portStr != "" {
		p, perr := strconv.Atoi(portStr)
		if perr != nil || p < 1 || p > 65535 {
			return aiEndpoint{}, &aiTransportError{code: "refused_address",
				message: "The server address is not a valid URL."}
		}
		effective = p
	} else if scheme == "https" {
		effective = 443
	} else {
		effective = 80
	}
	if policy.allowedPorts != nil && !policy.allowedPorts[effective] {
		return aiEndpoint{}, &aiTransportError{code: "refused_address",
			message: fmt.Sprintf("Port %d is not allowed.", effective)}
	}
	host := u.Hostname()
	if host == "" || u.User != nil {
		return aiEndpoint{}, &aiTransportError{code: "refused_address",
			message: "The server address needs a host and no user name or password."}
	}
	host = strings.TrimSuffix(host, ".")
	var addresses []string
	var privateOK bool
	literal := net.ParseIP(strings.Split(host, "%")[0]) != nil
	switch {
	case literal:
		addresses = []string{host}
		privateOK = policy.privateLiterals
	case !resolve:
		privateOK = true
	default:
		resolved, rerr := net.LookupIP(host)
		if rerr != nil || len(resolved) == 0 {
			return aiEndpoint{}, &aiTransportError{code: "unreachable",
				message: "The server's host name cannot be found."}
		}
		for _, ip := range resolved {
			addresses = append(addresses, ip.String())
		}
		privateOK = policy.privateHosts[strings.ToLower(host)]
	}
	if resolve && len(addresses) == 0 {
		return aiEndpoint{}, &aiTransportError{code: "unreachable",
			message: "The server's host name cannot be found."}
	}
	for _, addr := range addresses {
		ip := net.ParseIP(strings.Split(addr, "%")[0])
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return aiEndpoint{}, aiRefused()
		}
		if !privateOK && !isPublicIP(ip) {
			return aiEndpoint{}, aiRefused()
		}
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return aiEndpoint{scheme: scheme, host: host, port: effective, addresses: addresses, path: path}, nil
}

// isPublicIP ports remote.is_public.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false // CGNAT 100.64.0.0/10
		}
	}
	return ip.IsGlobalUnicast()
}

// aiReachable ports the transport's dial step for the run path: check under
// the policy, resolve, dial every checked address. All failing -> the
// probe-observed ai_unreachable; a successful connect hands the endpoint back
// for the (pending) provider exchange.
func (s *Settings) aiReachable(raw string, policy aiPolicy) (aiEndpoint, *aiTransportError) {
	ep, terr := s.aiCheckURL(raw, policy, true)
	if terr != nil {
		return aiEndpoint{}, terr
	}
	for _, addr := range ep.addresses {
		d := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := d.Dial("tcp", net.JoinHostPort(addr, strconv.Itoa(ep.port)))
		if err == nil {
			_ = conn.Close()
			return ep, nil
		}
	}
	return aiEndpoint{}, &aiTransportError{code: "unreachable", message: "The server cannot be reached."}
}

// statusForAICode maps apps/ai/errors.STATUS.
func statusForAICode(code string) int {
	switch code {
	case "timeout":
		return 504
	case "busy":
		return 503
	case "too_large":
		return 413
	case "not_supported":
		return 400
	default:
		return 502
	}
}

// aicallStatus ports services._status.
func aicallStatus(code string, refused bool) string {
	switch {
	case code == "timeout":
		return "timeout"
	case code == "refused_address" || code == "secret_unreadable" || refused:
		return "refused"
	default:
		return "error"
	}
}
