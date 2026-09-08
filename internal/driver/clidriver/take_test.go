package clidriver_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/driver/clidriver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTakeNamesTheBackupThenSyncs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Take("/w", "CLAUDE.md").
		Return(synccontroller.TakeReport{Path: "CLAUDE.md", Backup: "CLAUDE.md.bak"}, nil).Once()
	h.expectSync()

	require.NoError(t, h.driver.Run(t.Context(), []string{"take", "--root", "/w", "CLAUDE.md"}))
	assert.Contains(t, h.out.String(), "took CLAUDE.md")
	assert.Contains(t, h.out.String(), "the file on disk is kept at CLAUDE.md.bak")
	assert.Contains(t, h.out.String(), "synced /w")
}

func TestTakeWithNoFileIsRefusedByName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	err := h.driver.Run(t.Context(), []string{"take", "--root", "/w"})
	require.ErrorIs(t, err, clidriver.ErrUsage)
	assert.ErrorContains(t, err, "take names one rendered file")
}

func TestTakeWithTwoFilesIsRefusedByName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)

	err := h.driver.Run(t.Context(), []string{"take", "--root", "/w", "a.md", "b.md"})
	require.ErrorIs(t, err, clidriver.ErrUsage)
}

func TestTakeReportsTheControllersRefusal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.reads(factory)
	h.sync.EXPECT().Take("/w", "NOTES.md").
		Return(synccontroller.TakeReport{}, synccontroller.ErrNotRendered).Once()

	err := h.driver.Run(t.Context(), []string{"take", "--root", "/w", "NOTES.md"})
	require.ErrorIs(t, err, synccontroller.ErrNotRendered)
}

func TestBootstrapCarriesNoOverwritePolicyFlag(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	require.Error(t, h.driver.Run(t.Context(), []string{"bootstrap", "--force", "u"}))
	require.Error(t, h.driver.Run(t.Context(), []string{"bootstrap", "--backup", "u"}))
}

func TestUsageNamesTheTakeVerb(t *testing.T) {
	t.Parallel()

	assert.Contains(t, clidriver.Usage(), "take <file>")
	assert.NotContains(t, clidriver.Usage(), "--backup")
}
