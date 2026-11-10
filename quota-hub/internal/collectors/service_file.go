package collectors

import (
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"quota-hub/internal/model"
)

// serviceUsername is fixed by the OpenCode local service protocol.
const serviceUsername = "opencode"

// localOrigin accepts only a trusted local HTTP(S) origin: private/loopback IP,
// localhost or an internal .box name, without userinfo, path, query or fragment.
func localOrigin(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Zone() != "" || (!ip.IsPrivate() && !ip.IsLoopback()) {
			return "", false
		}
	} else if host != "localhost" && !strings.HasSuffix(host, ".box") {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// registration reads the OpenCode service registration and returns the
// credential endpoint and Authorization header. It is called for every fetch, so a
// restarted service with a new port or password is picked up without any CLI
// or refresh step. Nothing read here is stored or returned in errors.
func (o *openCodeKey) registration() (string, string, *model.ErrorInfo) {
	return o.registrationFor("/api/credential")
}

// registrationFor reads the same registration for another local endpoint of the
// same service. The path is a constant of this package, never a remote input.
func (o *openCodeKey) registrationFor(path string) (string, string, *model.ErrorInfo) {
	data, ok := readBounded(o.file, maxFileBytes)
	if !ok {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	defer zero(data)
	var reg struct {
		URL      string          `json:"url"`
		Password json.RawMessage `json:"password"`
	}
	if json.Unmarshal(data, &reg) != nil {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	origin := o.origin
	if origin == "" {
		if reg.URL == "" {
			return "", "", fail(CodeCredentialsUnavailable)
		}
		if origin, ok = localOrigin(reg.URL); !ok {
			return "", "", fail(CodeConfigInvalid)
		}
	}
	// A password is only decoded after the origin is known to be local.
	if len(reg.Password) == 0 {
		return origin + path, "", nil
	}
	var password string
	if json.Unmarshal(reg.Password, &password) != nil || string(reg.Password) == "null" {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(serviceUsername+":"+password))
	return origin + path, auth, nil
}
