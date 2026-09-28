package talos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Aliases --talos-version accepts in place of a release tag. They are resolved
// to a tag once, when a run starts, and only the tag is recorded: a rolling
// upgrade must not pick up a release published halfway through it.
const (
	// AliasLatest is the newest stable release, across minors.
	AliasLatest = "latest"
	// AliasLatestPatch is the newest stable patch of the minor already running.
	AliasLatestPatch = "latest-patch"
)

func IsAlias(v string) bool { return v == AliasLatest || v == AliasLatestPatch }

const releasesURL = "https://api.github.com/repos/siderolabs/talos/releases?per_page=100"

// stableTagRE excludes alphas, betas and release candidates. GitHub's
// prerelease flag says the same thing; checking the tag as well means a
// mis-flagged release still cannot slip in.
var stableTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// StableReleases lists published, non-prerelease Talos tags, newest first as
// GitHub returns them (not necessarily in version order: patches to older
// minors are interleaved).
func StableReleases(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list Talos releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list Talos releases: GitHub answered %s", resp.Status)
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("list Talos releases: %w", err)
	}
	var out []string
	for _, r := range rels {
		if !r.Draft && !r.Prerelease && stableTagRE.MatchString(r.Tag) {
			out = append(out, r.Tag)
		}
	}
	return out, nil
}

// PickRelease resolves an alias against a list of stable tags. running is the
// release the cluster runs now; latest-patch needs it, latest ignores it.
func PickRelease(tags []string, alias, running string) (string, error) {
	var want []int
	if alias == AliasLatestPatch {
		want = ParseVersion(running)
		if want == nil {
			return "", fmt.Errorf("cannot resolve %s: running release %q is not a version", alias, running)
		}
	} else if alias != AliasLatest {
		return "", fmt.Errorf("unknown alias %q", alias)
	}
	best, bestV := "", []int(nil)
	for _, t := range tags {
		v := ParseVersion(t)
		if v == nil || (want != nil && (v[0] != want[0] || v[1] != want[1])) {
			continue
		}
		if bestV == nil || CompareVersions(v, bestV) > 0 {
			best, bestV = t, v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no stable Talos release matches %s", alias)
	}
	return best, nil
}

// ParseVersion reads vMAJOR.MINOR.PATCH, ignoring a pre-release suffix.
// Anything else is nil.
func ParseVersion(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	f := strings.Split(v, ".")
	if len(f) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func CompareVersions(a, b []int) int {
	for i := range 3 {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
