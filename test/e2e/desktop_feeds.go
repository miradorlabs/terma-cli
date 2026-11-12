package e2e

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"slices"
	"sort"
	"time"
)

// The desktop apps' releases, as each app's update feed lists them: Claude Desktop's Squirrel
// RELEASES.json, the Codex app's Sparkle appcast.

// DesktopRelease is one release of a desktop app, as its update feed lists it.
type DesktopRelease struct {
	Version  string
	Build    string // the feed's build number, where it has one (Sparkle's)
	URL      string
	Released time.Time
}

const (
	claudeDesktopFeed = "https://downloads.claude.ai/releases/darwin/universal/RELEASES.json"
	codexDesktopFeed  = "https://persistent.oaistatic.com/codex-app-prod/appcast.xml"
)

// DesktopFeeds are the update feeds the desktop apps' updaters read, by harness.
var DesktopFeeds = map[string]string{"claude-desktop": claudeDesktopFeed, "codex-desktop": codexDesktopFeed}

// ParseDesktopFeed reads the releases harness's update feed lists, oldest first.
func ParseDesktopFeed(harness string, data []byte) ([]DesktopRelease, error) {
	if harness == "claude-desktop" {
		return parseSquirrelFeed(data)
	}
	return parseAppcast(data)
}

// claudeDesktopReleases are the releases Claude Desktop's updater offers (Squirrel's
// RELEASES.json): the current one alone.
func claudeDesktopReleases() ([]DesktopRelease, error) {
	data, err := fetch(claudeDesktopFeed)
	if err != nil {
		return nil, err
	}
	return parseSquirrelFeed(data)
}

// parseSquirrelFeed reads a Squirrel.Mac RELEASES.json.
func parseSquirrelFeed(data []byte) ([]DesktopRelease, error) {
	var feed struct {
		Releases []struct {
			UpdateTo struct {
				Version string    `json:"version"`
				URL     string    `json:"url"`
				PubDate time.Time `json:"pub_date"`
			} `json:"updateTo"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("RELEASES.json: %w", err)
	}
	var out []DesktopRelease
	for _, r := range feed.Releases {
		if u := r.UpdateTo; u.Version != "" && u.URL != "" {
			// A release's version names the folder it is unpacked in, and its published files.
			if err := CheckPathName("Claude Desktop's RELEASES.json version", u.Version); err != nil {
				return nil, err
			}
			out = append(out, DesktopRelease{Version: u.Version, URL: u.URL, Released: u.PubDate})
		}
	}
	return sortReleases(out), nil
}

// codexDesktopReleases are the releases the Codex app's Sparkle feed lists, its full archives
// (not the deltas between them).
func codexDesktopReleases() ([]DesktopRelease, error) {
	data, err := fetch(codexDesktopFeed)
	if err != nil {
		return nil, err
	}
	return parseAppcast(data)
}

// parseAppcast reads a Sparkle appcast's items.
func parseAppcast(data []byte) ([]DesktopRelease, error) {
	var feed struct {
		Items []struct {
			PubDate   string `xml:"pubDate"`
			Version   string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle version"`
			Short     string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle shortVersionString"`
			Enclosure struct {
				URL string `xml:"url,attr"`
			} `xml:"enclosure"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}
	var out []DesktopRelease
	for _, it := range feed.Items {
		if it.Short == "" || it.Enclosure.URL == "" {
			continue
		}
		if err := CheckPathName("the appcast's version", it.Short); err != nil {
			return nil, err
		}
		at, _ := time.Parse(time.RFC1123Z, it.PubDate)
		out = append(out, DesktopRelease{Version: it.Short, Build: it.Version, URL: it.Enclosure.URL, Released: at.UTC()})
	}
	return sortReleases(out), nil
}

// sortReleases orders releases oldest first, one a version.
func sortReleases(rs []DesktopRelease) []DesktopRelease {
	sort.SliceStable(rs, func(i, j int) bool { return versionLess(rs[i].Version, rs[j].Version) })
	return slices.CompactFunc(rs, func(a, b DesktopRelease) bool { return a.Version == b.Version })
}
