package gomod_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/util/gomod"
	"github.com/stretchr/testify/assert"
)

const member = `module example.com/member

go 1.25

require example.com/single v1.0.0 // indirect

require (
	// a block comment
	example.com/declared v1.1.0
	example.com/undeclared v2.0.0
	example.com/indirect v0.3.0 // indirect
)

replace example.com/declared => ../declared

exclude example.com/declared v1.0.9

exclude (
	example.com/indirect v0.3.1
)
`

func TestSetVersionsMovesOnlyTheRequiresTheMapNames(t *testing.T) {
	t.Parallel()

	got := gomod.SetVersions(member, map[string]string{
		"example.com/single":   "v1.0.5",
		"example.com/declared": "v1.2.0",
		"example.com/indirect": "v0.4.0",
		"example.com/absent":   "v9.0.0",
	})

	assert.Equal(t, `module example.com/member

go 1.25

require example.com/single v1.0.5 // indirect

require (
	// a block comment
	example.com/declared v1.2.0
	example.com/undeclared v2.0.0
	example.com/indirect v0.4.0 // indirect
)

replace example.com/declared => ../declared

exclude example.com/declared v1.0.9

exclude (
	example.com/indirect v0.3.1
)
`, got)
}

func TestRequiresNamesEverySingleAndBlockRequire(t *testing.T) {
	t.Parallel()

	assert.Equal(t, map[string]bool{
		"example.com/single":     true,
		"example.com/declared":   true,
		"example.com/undeclared": true,
		"example.com/indirect":   true,
	}, gomod.Requires(member))
}
