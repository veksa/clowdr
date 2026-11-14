package ingress

import (
	"net"
	"net/http"
	"regexp"
	"strings"
)

// FromAPISocket reports whether r came over clowdrd's unix socket, that is through nginx.
func FromAPISocket(r *http.Request) bool {
	_, _, err := net.SplitHostPort(r.RemoteAddr)
	return err != nil // a unix socket peer is "" or "@"
}

// FromDeployHost reports whether r was passed by nginx's deploy-host location, the one place
// that sets ViaHeader.
func FromDeployHost(r *http.Request) bool {
	return FromAPISocket(r) && r.Header.Get(ViaHeader) == ViaDeployHost
}

var acmeTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// IsACMEChallenge reports whether r is a plain GET of one HTTP-01 challenge token, with no
// escapes or further path.
func IsACMEChallenge(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.URL.EscapedPath(), ACMEChallenge)
	return ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) && acmeTokenRe.MatchString(tok)
}
