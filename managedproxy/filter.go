package managedproxy

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

type providerProxy struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Filtering and deduplication use original names, not the native provider prefix.
// A matched dedupe expression uses its capture tuple (whole match if no captures).
// Unmatched names remain independent. Preference ties resolve lexically, never by IP.
func selectNames(s Subscription, proxies []providerProxy) ([]string, int, error) {
	compile := func(p string) (*regexp.Regexp, error) {
		if p == "" {
			return nil, nil
		}
		return regexp.Compile(p)
	}
	inc, err := compile(s.IncludeRegex)
	if err != nil {
		return nil, 0, errors.New("managed proxy: invalid include expression")
	}
	exc, err := compile(s.ExcludeRegex)
	if err != nil {
		return nil, 0, errors.New("managed proxy: invalid exclude expression")
	}
	dup, err := compile(s.DedupeRegex)
	if err != nil {
		return nil, 0, errors.New("managed proxy: invalid dedupe expression")
	}
	prefer, err := compile(s.PreferRegex)
	if err != nil {
		return nil, 0, errors.New("managed proxy: invalid preference expression")
	}
	groups := map[string]string{}
	seen := map[string]bool{}
	total := 0
	for _, p := range proxies {
		switch strings.ToLower(p.Type) {
		case "direct", "reject", "rejectdrop", "reject-drop", "compatible", "pass":
			continue
		}
		if !strings.HasPrefix(p.Name, prefix(s.ID)) {
			return nil, 0, errors.New("managed proxy: core lacks required provider name prefix support")
		}
		name := strings.TrimPrefix(p.Name, prefix(s.ID))
		if name == "" || strings.ContainsRune(name, '\x00') {
			return nil, 0, errors.New("managed proxy: provider returned an invalid node name")
		}
		if seen[name] {
			return nil, 0, errors.New("managed proxy: provider returned duplicate full node names")
		}
		seen[name] = true
		total++
		if inc != nil && !inc.MatchString(name) || exc != nil && exc.MatchString(name) {
			continue
		}
		key := "name\x00" + name
		if dup != nil {
			match := dup.FindStringSubmatch(name)
			if len(match) > 1 {
				key = "group\x00" + strings.Join(match[1:], "\x00")
			} else if len(match) == 1 {
				key = "group\x00" + match[0]
			}
		}
		old, ok := groups[key]
		if !ok {
			groups[key] = name
			continue
		}
		newPreferred, oldPreferred := prefer != nil && prefer.MatchString(name), prefer != nil && prefer.MatchString(old)
		if newPreferred && !oldPreferred || newPreferred == oldPreferred && name < old {
			groups[key] = name
		}
	}
	names := make([]string, 0, len(groups))
	for _, n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, total, nil
}
