package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/history"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/tui"
)

// catalogHydrator refreshes package metadata and, when its definition belongs
// to a repository Cicerone already tracks, indexes a bounded slice of that
// definition's history. Other taps still gain package info without cloning a
// new repository or inventing an update event.
type catalogHydrator struct {
	details    *packageDetailLoader
	store      *store.Store
	repository func(context.Context, string) (gitrepo.Repository, error)
	slots      chan struct{}
}

func (h catalogHydrator) HydrateCatalog(ctx context.Context, id domain.PackageID) error {
	if h.slots != nil {
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var info homebrew.PackageInfo
	if cached, found, cacheErr := h.store.PackageInfo(ctx, string(id)); cacheErr == nil && found &&
		time.Since(cached.FetchedAt) < 5*time.Minute {
		_ = json.Unmarshal(cached.Normalized, &info)
	}
	if info.FullName == "" || info.Tap == "" || info.SourcePath == "" {
		var err error
		info, err = h.details.refreshPackageInfo(ctx, id)
		if err != nil {
			return err
		}
	}
	if h.details.send != nil {
		h.details.send(tui.DatasetChanged{})
	}
	var source gitrepo.Source
	switch info.Tap {
	case "homebrew/core":
		source = gitrepo.Source{Name: "homebrew-core", Kind: "formula"}
	case "homebrew/cask":
		source = gitrepo.Source{Name: "homebrew-cask", Kind: "cask"}
	default:
		return nil
	}
	if info.SourcePath == "" || h.repository == nil {
		return nil
	}
	repo, err := h.repository(ctx, source.Name)
	if err != nil {
		return err // Metadata has already refreshed; retry history after a later feed change.
	}
	actual := repo.Source()
	if actual.Name != source.Name || actual.Kind != source.Kind {
		return nil
	}
	_, err = history.NewIndexer(repo, h.store).IndexPackage(ctx, actual, id, info.SourcePath)
	return err
}
