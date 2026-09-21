// Package match implements local, accent-insensitive fuzzy matching of a
// free-text query against stop names.
package match

import (
	"sort"
	"strings"
	"unicode"
)

type Stop struct {
	ID   string
	Name string
}

type Candidate struct {
	Name string
	IDs  []string
}

func Normalize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	space := true
	for _, r := range strings.ToLower(s) {
		if a, ok := accents[r]; ok {
			r = a
		}
		if unicode.IsSpace(r) || r == '-' || r == '/' || r == ',' || r == '.' || r == '(' || r == ')' {
			if !space {
				sb.WriteByte(' ')
				space = true
			}
			continue
		}
		space = false
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}

var accents = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'ő': 'o', 'õ': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u', 'ű': 'u',
	'ç': 'c', 'ñ': 'n', 'ß': 's',
	'æ': 'a', 'ø': 'o', 'œ': 'o', 'ð': 'd', 'þ': 't',
	'č': 'c', 'ć': 'c', 'ř': 'r', 'š': 's', 'ś': 's', 'ž': 'z', 'ź': 'z', 'ż': 'z',
	'ě': 'e', 'ę': 'e', 'ý': 'y', 'ů': 'u', 'ł': 'l', 'ń': 'n', 'ą': 'a', 'ď': 'd', 'ť': 't', 'ň': 'n',
	'ğ': 'g', 'ş': 's', 'ı': 'i',
}

func Group(stops []Stop) []Candidate {
	var out []Candidate
	idx := map[string]int{}
	for _, s := range stops {
		k := Normalize(s.Name)
		if k == "" {
			continue
		}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, Candidate{Name: s.Name})
			i = len(out) - 1
		}
		if !contains(out[i].IDs, s.ID) {
			out[i].IDs = append(out[i].IDs, s.ID)
		}
	}
	return out
}

// Tiers, best first: exact name, name prefix, substring, edit distance.
// One result is a match; none or several is not.
func Find(query string, stops []Stop) []Candidate {
	q := Normalize(query)
	if q == "" {
		return nil
	}
	cands := Group(stops)
	var exact, prefix, sub, fuzzy []Candidate
	for _, c := range cands {
		n := Normalize(c.Name)
		switch {
		case n == q:
			exact = append(exact, c)
		case strings.HasPrefix(n, q):
			prefix = append(prefix, c)
		case strings.Contains(n, q):
			sub = append(sub, c)
		case fuzzyMatch(q, n):
			fuzzy = append(fuzzy, c)
		}
	}
	switch {
	case len(exact) > 0:
		return exact
	case len(prefix) > 0:
		return prefix
	case len(sub) > 0:
		return sub
	default:
		return fuzzy
	}
}

func Closest(query string, stops []Stop, n int) []Candidate {
	q := Normalize(query)
	cands := Group(stops)
	type scored struct {
		c Candidate
		d int
	}
	s := make([]scored, 0, len(cands))
	for _, c := range cands {
		s = append(s, scored{c, bestDistance(q, Normalize(c.Name))})
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].d < s[j].d })
	if len(s) > n {
		s = s[:n]
	}
	out := make([]Candidate, len(s))
	for i := range s {
		out[i] = s[i].c
	}
	return out
}

func fuzzyMatch(q, name string) bool {
	thr := len([]rune(q)) / 4
	if thr < 1 {
		thr = 1
	}
	return bestDistance(q, name) <= thr
}

func bestDistance(q, name string) int {
	best := Levenshtein(q, name)
	qw := strings.Fields(q)
	nw := strings.Fields(name)
	for i := 0; i+len(qw) <= len(nw); i++ {
		if d := Levenshtein(q, strings.Join(nw[i:i+len(qw)], " ")); d < best {
			best = d
		}
	}
	return best
}

func Levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
