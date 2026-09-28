package talos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// manifestAccept covers both OCI and Docker indexes/manifests; the installer
// images are published as multi-arch indexes.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// ImageExists asks the registry whether an image reference resolves, without
// pulling it. It exists so a mistyped --talos-version is refused by preflight,
// before the node is drained, rather than by the pull inside talos-upgrade.
//
// known is false when the registry could not give a straight answer (network,
// auth scheme it does not understand); callers should warn, not fail, then.
func ImageExists(ctx context.Context, ref string) (exists, known bool, err error) {
	host, repo, tag, err := splitRef(ref)
	if err != nil {
		return false, false, err
	}
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)
	hc := &http.Client{Timeout: 15 * time.Second}

	head := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", manifestAccept)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return hc.Do(req)
	}
	resp, err := head("")
	if err != nil {
		return false, false, err
	}
	resp.Body.Close()
	// ghcr.io and most others want an anonymous bearer token even for public
	// images; the challenge says where to get one.
	if resp.StatusCode == http.StatusUnauthorized {
		token, terr := anonymousToken(ctx, hc, resp.Header.Get("WWW-Authenticate"))
		if terr != nil {
			return false, false, terr
		}
		if resp, err = head(token); err != nil {
			return false, false, err
		}
		resp.Body.Close()
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true, nil
	case http.StatusNotFound:
		return false, true, nil
	}
	return false, false, fmt.Errorf("%s answered %s", host, resp.Status)
}

// splitRef handles host/repo:tag. A reference without a registry host is on
// Docker Hub, which pvectl has no reason to support.
func splitRef(ref string) (host, repo, tag string, err error) {
	if strings.Contains(ref, "@") {
		d := strings.SplitN(ref, "@", 2)
		ref, tag = d[0], d[1]
	}
	slash := strings.IndexByte(ref, '/')
	if slash < 0 || !strings.ContainsAny(ref[:slash], ".:") {
		return "", "", "", fmt.Errorf("image %q does not name a registry", ref)
	}
	host, repo = ref[:slash], ref[slash+1:]
	if tag == "" {
		i := strings.LastIndexByte(repo, ':')
		if i < 0 {
			return "", "", "", fmt.Errorf("image %q has no tag", ref)
		}
		repo, tag = repo[:i], repo[i+1:]
	}
	return host, repo, tag, nil
}

// anonymousToken follows a `Bearer realm=...,service=...,scope=...` challenge.
func anonymousToken(ctx context.Context, hc *http.Client, challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", fmt.Errorf("unsupported registry auth challenge %q", challenge)
	}
	params := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(challenge, "Bearer "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			params[k] = strings.Trim(v, `"`)
		}
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry auth challenge has no realm")
	}
	q := url.Values{}
	for _, k := range []string{"service", "scope"} {
		if params[k] != "" {
			q.Set(k, params[k])
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint answered %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}
