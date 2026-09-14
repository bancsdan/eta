// Package all registers every city provider. cmd/eta imports it for the
// side effect; tests that need a single city import that city directly.
package all

import (
	_ "github.com/bancsdan/eta/internal/providers/bkk"
	_ "github.com/bancsdan/eta/internal/providers/bvg"
	_ "github.com/bancsdan/eta/internal/providers/entur"
	_ "github.com/bancsdan/eta/internal/providers/mbta"
	_ "github.com/bancsdan/eta/internal/providers/tfl"
)
