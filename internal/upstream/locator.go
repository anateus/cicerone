package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"cicerone/internal/store"
)

type FetchedPage struct {
	FinalURL  *url.URL
	MediaType string
	Body      []byte
}

type Locator struct {
	Store *store.Store
	Fetch func(context.Context, string) (FetchedPage, error)
	Now   func() time.Time
}

func (l *Locator) Resolve(ctx context.Context, packageID, packageName string, values ...string) (string, error) {
	if l == nil || l.Store == nil {
		return "", errors.New("repository locator store is nil")
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	for _, value := range values {
		if repositoryURL, ok := CanonicalRepository(value); ok {
			err := l.Store.SavePackageRepository(ctx, store.PackageRepository{
				PackageID: packageID, URL: repositoryURL, SourceURL: value,
				Confidence: 1, DiscoveredAt: now().UTC(),
			})
			return repositoryURL, err
		}
	}
	if cached, ok, err := l.Store.PackageRepository(ctx, packageID); err != nil {
		return "", err
	} else if ok {
		return cached.URL, nil
	}
	if l.Fetch == nil {
		return "", errors.New("repository homepage fetch is unavailable")
	}
	var failures []error
	for _, value := range values {
		seed, err := url.Parse(value)
		if err != nil || seed.Hostname() == "" || (seed.Scheme != "http" && seed.Scheme != "https") {
			continue
		}
		page, err := l.Fetch(ctx, value)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		base := page.FinalURL
		if base == nil {
			base = seed
		}
		repositoryURL, ok := DiscoverRepository(base, page.Body, packageName)
		if !ok {
			failures = append(failures, fmt.Errorf("%s has no reliable repository link", value))
			continue
		}
		if err := l.Store.SavePackageRepository(ctx, store.PackageRepository{
			PackageID: packageID, URL: repositoryURL, SourceURL: base.String(),
			Confidence: 1, DiscoveredAt: now().UTC(),
		}); err != nil {
			return "", err
		}
		return repositoryURL, nil
	}
	if len(failures) == 0 {
		return "", errors.New("no repository homepage seed")
	}
	return "", errors.Join(failures...)
}
