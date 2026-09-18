// Package all registers every provider. cmd/eta imports it for the side
// effect; tests that need a single provider import that package directly.
package all

import (
	_ "github.com/bancsdan/eta/internal/providers/bkk"
	_ "github.com/bancsdan/eta/internal/providers/bvg"
	_ "github.com/bancsdan/eta/internal/providers/digitransit"
	_ "github.com/bancsdan/eta/internal/providers/entur"
	_ "github.com/bancsdan/eta/internal/providers/mbta"
	_ "github.com/bancsdan/eta/internal/providers/mta"
	_ "github.com/bancsdan/eta/internal/providers/opendatach"
	_ "github.com/bancsdan/eta/internal/providers/ovapi"
	_ "github.com/bancsdan/eta/internal/providers/sl"
	_ "github.com/bancsdan/eta/internal/providers/tfi"
	_ "github.com/bancsdan/eta/internal/providers/tfl"
	_ "github.com/bancsdan/eta/internal/providers/wienerlinien"
)
