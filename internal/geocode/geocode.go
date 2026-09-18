// Package geocode turns a town name into a point and a radius, for
// providers whose stop data carries no town. It asks Nominatim
// (OpenStreetMap) once per town and caches the answer for a month, which
// keeps well inside its one-request-per-second policy.
package geocode

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

const (
	nominatim = "https://nominatim.openstreetmap.org"
	userAgent = "eta/1 (+https://github.com/bancsdan/eta)"
	cacheTTL  = 30 * 24 * time.Hour
	minRadius = 4.0
)

// Place is a located town.
type Place struct {
	Name     string  `json:"name"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	RadiusKm float64 `json:"radiusKm"` // half the diagonal of its bounding box, clamped to [4, NearbyKm]
}

// Contains reports whether a point lies within the place's radius.
func (p Place) Contains(lat, lon float64) bool {
	return xutil.DistanceKm(p.Lat, p.Lon, lat, lon) <= p.RadiusKm
}

// Nominatim is a geocoder; the zero value uses the public instance and a
// shared on-disk cache.
type Nominatim struct {
	BaseURL string
	HTTP    *httpx.Client
	Cache   *cache.Cache
}

type result struct {
	DisplayName string    `json:"display_name"`
	Name        string    `json:"name"`
	Lat         string    `json:"lat"`
	Lon         string    `json:"lon"`
	BoundingBox []string  `json:"boundingbox"`
	Importance  float64   `json:"importance"`
	Address     addressOf `json:"address"`
}

type addressOf struct {
	City    string `json:"city"`
	Town    string `json:"town"`
	Village string `json:"village"`
}

// Locate finds town within the ISO country code. It fails with
// transit.ErrNotFound when nothing matches.
func (n *Nominatim) Locate(ctx context.Context, town, countryCode string) (Place, error) {
	key := "geocode-" + countryCode + "-" + strings.ReplaceAll(match.Normalize(town), " ", "-")
	c := n.Cache
	if c == nil {
		c = cache.Default("geocode", cacheTTL)
	}
	var p Place
	if c.Load(key, &p) && p.RadiusKm > 0 {
		return p, nil
	}
	h := n.HTTP
	if h == nil {
		h = httpx.New("nominatim", nil)
	}
	h.Header.Set("User-Agent", userAgent)
	base := n.BaseURL
	if base == "" {
		base = nominatim
	}
	q := url.Values{"q": {town}, "format": {"jsonv2"}, "limit": {"5"}, "countrycodes": {countryCode}, "addressdetails": {"1"}}
	var rs []result
	if err := h.GetJSON(ctx, httpx.URL(base, "/search", q), &rs); err != nil {
		return Place{}, err
	}
	best := pick(rs, town)
	if best == nil {
		return Place{}, fmt.Errorf("geocode: no place named %q in %s: %w", town, strings.ToUpper(countryCode), transit.ErrNotFound)
	}
	fmt.Sscan(best.Lat, &p.Lat) //nolint:errcheck
	fmt.Sscan(best.Lon, &p.Lon) //nolint:errcheck
	p.Name = best.Name
	if p.Name == "" {
		p.Name, _, _ = strings.Cut(best.DisplayName, ",")
	}
	p.RadiusKm = minRadius
	if len(best.BoundingBox) == 4 {
		var s, nn, w, e float64
		fmt.Sscan(best.BoundingBox[0], &s)  //nolint:errcheck
		fmt.Sscan(best.BoundingBox[1], &nn) //nolint:errcheck
		fmt.Sscan(best.BoundingBox[2], &w)  //nolint:errcheck
		fmt.Sscan(best.BoundingBox[3], &e)  //nolint:errcheck
		p.RadiusKm = math.Max(minRadius, math.Min(xutil.NearbyKm, xutil.DistanceKm(s, w, nn, e)/2))
	}
	_ = c.Store(key, p)
	return p, nil
}

// pick prefers a settlement whose own name matches the query over a street
// or estate that merely contains it, then the most important result.
func pick(rs []result, town string) *result {
	want := match.Normalize(town)
	var best *result
	score := func(r *result) int {
		s := 0
		if match.Normalize(r.Name) == want {
			s += 2
		}
		for _, a := range []string{r.Address.City, r.Address.Town, r.Address.Village} {
			if a != "" && match.Normalize(a) == want {
				s++
				break
			}
		}
		return s
	}
	for i := range rs {
		r := &rs[i]
		if best == nil || score(r) > score(best) || (score(r) == score(best) && r.Importance > best.Importance) {
			best = r
		}
	}
	return best
}
