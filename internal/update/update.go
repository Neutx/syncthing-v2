// Package update checks GitHub Releases for a newer SyncThing V2 (spec §2.3,
// §4.6 T15). It only reads: one unauthenticated GET of the repository's
// latest release, at most once per 24 hours, and it never downloads or
// installs anything. Drafts and prereleases are ignored. The time of the last
// check and the release it found are recorded in prefs, and the check is
// skipped entirely when the user turned "Check for updates" off.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/prefs"
)

const (
	// Interval is the minimum time between two release queries.
	Interval = 24 * time.Hour
	// Timeout bounds one release query.
	Timeout = 5 * time.Second
	// maxBody bounds how much of the API response is read.
	maxBody = 1 << 20
)

// LatestURL is the GitHub REST endpoint for the repository's latest release.
var LatestURL = "https://api.github.com/repos/" + brand.RepoOwner + "/" + brand.RepoName + "/releases/latest"

// ReleasesPage is the public releases page, used when a release has no URL.
const ReleasesPage = brand.RepoURL + "/releases"

// ErrNoRelease means the repository has no published, non-prerelease release.
var ErrNoRelease = errors.New("no published SyncThing V2 release")

// Release is a published release.
type Release struct {
	Version string // normalised "vX.Y.Z"
	Name    string
	URL     string // release page
}

// Result is the outcome of Check.
type Result struct {
	// Latest is the newest known release; zero when none is known.
	Latest Release
	// Available is true when Latest is newer than the running version.
	Available bool
	// Checked is true when this call queried GitHub; false when the result
	// came from the last check because the 24 h interval has not passed.
	Checked bool
	// Notify is true when Available and this version was not announced yet
	// (prefs UpdateNotified). Call MarkNotified after showing the notice.
	Notify bool
}

// Checker runs the throttled release check. The zero value is not usable;
// build one with New.
type Checker struct {
	// URL is the latest-release endpoint (LatestURL in production).
	URL string
	// Current is the running version (brand.Version in production).
	Current string
	// Client performs the request; its own timeout is not relied on, every
	// request carries a RequestTimeout deadline.
	Client *http.Client
	// RequestTimeout bounds one query; 0 means Timeout.
	RequestTimeout time.Duration
	// Prefs records the last check and the announced version. It may be nil,
	// in which case the interval is tracked in memory only.
	Prefs *prefs.Store
	// Interval between queries (Interval in production).
	Interval time.Duration
	// Now returns the current time (time.Now in production).
	Now func() time.Time

	mu        sync.Mutex
	last      *Result
	lastQuery time.Time // used when Prefs is nil
}

// New returns a Checker for the real repository that records its state in p.
func New(p *prefs.Store) *Checker {
	return &Checker{
		URL:            LatestURL,
		Current:        brand.Version,
		Client:         &http.Client{},
		RequestTimeout: Timeout,
		Prefs:          p,
		Interval:       Interval,
		Now:            time.Now,
	}
}

type apiRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Latest queries the latest release once, without the 24 h throttle and
// without recording anything. Drafts, prereleases and tags that carry a
// semver prerelease suffix yield ErrNoRelease.
func (c *Checker) Latest(ctx context.Context) (Release, error) {
	d := c.RequestTimeout
	if d <= 0 {
		d = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", brand.UserAgent())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update check: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Release{}, fmt.Errorf("update check: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Release{}, ErrNoRelease
	case resp.StatusCode != http.StatusOK:
		return Release{}, &StatusError{Status: resp.StatusCode}
	}
	var r apiRelease
	if err := json.Unmarshal(body, &r); err != nil {
		return Release{}, fmt.Errorf("update check: bad response: %w", err)
	}
	if r.Draft || r.Prerelease {
		return Release{}, ErrNoRelease
	}
	v, err := Parse(r.TagName)
	if err != nil {
		return Release{}, fmt.Errorf("update check: release tag %q: %w", r.TagName, err)
	}
	if v.Pre != "" {
		return Release{}, ErrNoRelease
	}
	rel := Release{Version: v.String(), Name: r.Name, URL: r.HTMLURL}
	if !strings.HasPrefix(rel.URL, "https://") {
		rel.URL = ReleasesPage
	}
	return rel, nil
}

// StatusError is an unexpected HTTP status from the releases API.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return "update check: GitHub answered HTTP " + strconv.Itoa(e.Status)
}

// Due reports whether the interval since the last query has passed. It is
// false when update checks are turned off in prefs.
func (c *Checker) Due() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled() && c.dueLocked()
}

func (c *Checker) enabled() bool {
	return c.Prefs == nil || c.Prefs.Get().CheckForUpdates
}

func (c *Checker) lastQueryTime() time.Time {
	if c.Prefs != nil {
		return c.Prefs.Get().LastUpdateCheck
	}
	return c.lastQuery
}

func (c *Checker) dueLocked() bool {
	last := c.lastQueryTime()
	if last.IsZero() {
		return true
	}
	since := c.Now().Sub(last)
	// A clock that went backwards (last check "in the future") must not
	// block checks forever.
	return since >= c.Interval || since < 0
}

// Check runs the release check if it is due and otherwise returns the last
// known result. When checks are turned off in prefs it returns a zero Result
// and no error, without any network access. A query that received an HTTP
// answer (including "no release") is recorded as the last check; a network
// failure is not, so the next Check retries.
func (c *Checker) Check(ctx context.Context) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled() {
		return Result{}, nil
	}
	if !c.dueLocked() {
		return c.cachedLocked(), nil
	}
	rel, err := c.Latest(ctx)
	var se *StatusError
	answered := err == nil || errors.Is(err, ErrNoRelease) || errors.As(err, &se)
	if answered {
		// An HTTP error says nothing about the latest release, so the one
		// recorded before is kept; "no release" clears it.
		var latest *string
		switch {
		case err == nil:
			latest = &rel.Version
		case errors.Is(err, ErrNoRelease):
			latest = new(string)
		}
		if rerr := c.recordLocked(latest); rerr != nil && err == nil {
			err = rerr
		}
	}
	if errors.Is(err, ErrNoRelease) {
		res := Result{Checked: true}
		c.last = &res
		return res, nil
	}
	if err != nil {
		return c.cachedLocked(), err
	}
	res := c.resultFor(rel)
	res.Checked = true
	c.last = &res
	return res, nil
}

// recordLocked records the time of an answered query and, when latest is
// not nil, the release it found ("" for none).
func (c *Checker) recordLocked(latest *string) error {
	now := c.Now()
	if c.Prefs == nil {
		c.lastQuery = now
		return nil
	}
	return c.Prefs.Update(func(p *prefs.Prefs) error {
		p.LastUpdateCheck = now
		if latest != nil {
			p.LatestRelease = *latest
		}
		return nil
	})
}

func (c *Checker) resultFor(rel Release) Result {
	res := Result{Latest: rel}
	newer, err := Newer(rel.Version, c.Current)
	if err != nil || !newer {
		return res
	}
	res.Available = true
	res.Notify = c.Prefs == nil || c.Prefs.Get().UpdateNotified != rel.Version
	return res
}

// cachedLocked returns the last result of this process, or, in another
// process (a restart, or `stv2 doctor` next to the tray), the release the
// last recorded check found (prefs LatestRelease) while it is still newer
// than the running one. Notify is set as for a fresh result, so a release
// that was found but not yet announced is still announced once.
func (c *Checker) cachedLocked() Result {
	var latest Release
	switch {
	case c.last != nil:
		latest = c.last.Latest
	case c.Prefs != nil:
		latest = Release{Version: c.Prefs.Get().LatestRelease, URL: ReleasesPage}
	}
	if latest.Version == "" {
		return Result{}
	}
	return c.resultFor(latest)
}

// MarkNotified records that the notice for version v was shown, so Notify is
// false for it from now on, also after a restart.
func (c *Checker) MarkNotified(v string) error {
	if c.Prefs == nil {
		return nil
	}
	return c.Prefs.Update(func(p *prefs.Prefs) error {
		p.UpdateNotified = v
		return nil
	})
}

// pollEvery is how often Run re-evaluates whether a check is due.
const pollEvery = time.Hour

// Run checks at once and then re-evaluates every hour until ctx ends; the
// 24 h throttle and the prefs switch apply to every attempt. fn receives each
// successful result.
func (c *Checker) Run(ctx context.Context, fn func(Result)) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		if res, err := c.Check(ctx); err == nil && fn != nil {
			fn(res)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // prerelease identifiers without the '-'; "" for a release
}

// String formats v as "vX.Y.Z[-pre]".
func (v Version) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Parse parses "X.Y.Z", "vX.Y.Z", with an optional "-prerelease" and
// "+build" suffix (build metadata is ignored).
func Parse(s string) (Version, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Version
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, v.Pre = s[:i], s[i+1:]
		if v.Pre == "" {
			return Version{}, errors.New("empty prerelease")
		}
		for _, id := range strings.Split(v.Pre, ".") {
			if id == "" {
				return Version{}, errors.New("empty prerelease identifier")
			}
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%q is not a MAJOR.MINOR.PATCH version", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("%q is not a MAJOR.MINOR.PATCH version", s)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// Compare returns -1, 0 or +1 following semver precedence: a prerelease
// sorts before its release, and prerelease identifiers compare numerically
// when both are numbers and lexically otherwise.
func Compare(a, b Version) int {
	for _, d := range [3][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			return cmp(d[0], d[1])
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	x, y := strings.Split(a.Pre, "."), strings.Split(b.Pre, ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		xn, xerr := strconv.Atoi(x[i])
		yn, yerr := strconv.Atoi(y[i])
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return cmp(xn, yn)
			}
		case xerr == nil: // numeric identifiers sort before alphanumeric ones
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x[i], y[i]); c != 0 {
				return c
			}
		}
	}
	return cmp(len(x), len(y))
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Newer reports whether release version candidate is newer than current.
// A candidate with a prerelease suffix is never newer: prereleases are not
// offered.
func Newer(candidate, current string) (bool, error) {
	c, err := Parse(candidate)
	if err != nil {
		return false, err
	}
	if c.Pre != "" {
		return false, nil
	}
	cur, err := Parse(current)
	if err != nil {
		return false, err
	}
	return Compare(c, cur) > 0, nil
}
