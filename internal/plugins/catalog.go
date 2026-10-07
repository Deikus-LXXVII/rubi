package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/integrity"
	"github.com/Deikus-LXXVII/rubi/internal/update"
)

// DefaultCatalog is the reviewed plugin catalog, signed with the Rubi release key.
const DefaultCatalog = "https://rubi-panel.com/marketplace/catalog.json"

type Catalog struct {
	Schema    int            `json:"schema"`
	UpdatedAt time.Time      `json:"updated_at"`
	Plugins   []CatalogEntry `json:"plugins"`
}

type CatalogEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Summary   string `json:"summary"`
	Publisher struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	} `json:"publisher"`
	Source   string           `json:"source"`
	Versions []CatalogVersion `json:"versions"`
}

type CatalogVersion struct {
	Version    string `json:"version"`
	SumsSHA256 string `json:"sums_sha256"`
	MinRubi    string `json:"min_rubi,omitempty"`
}

// CatalogURL returns where the catalog is read from; test builds may override it.
func CatalogURL(current string) string {
	if current == "dev" || strings.HasSuffix(current, "-test") {
		if u := os.Getenv("RUBI_MARKETPLACE_URL"); u != "" {
			return u
		}
	}
	return DefaultCatalog
}

// FetchCatalog downloads the catalog and verifies its signature with the Rubi release key.
func FetchCatalog(ctx context.Context, current string) (*Catalog, error) {
	u := CatalogURL(current)
	body, err1 := integrity.Fetch(ctx, u)
	sig, err2 := integrity.Fetch(ctx, u+".sig")
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("plugin catalog: %w", err)
	}
	return ParseCatalog(integrity.TrustedKey(current), body, sig)
}

// ParseCatalog verifies and decodes a catalog.
func ParseCatalog(releaseKeyPEM, body, sig []byte) (*Catalog, error) {
	if err := integrity.Verify(releaseKeyPEM, body, sig); err != nil {
		return nil, errors.New("plugin catalog is not signed with the Rubi release key")
	}
	var c Catalog
	if err := json.Unmarshal(body, &c); err != nil || c.Schema != 1 {
		return nil, errors.New("plugin catalog: unsupported format")
	}
	return &c, nil
}

// Entry finds a plugin in the catalog.
func (c *Catalog) Entry(id string) (*CatalogEntry, bool) {
	if c == nil {
		return nil, false
	}
	for i := range c.Plugins {
		if c.Plugins[i].ID == id {
			return &c.Plugins[i], true
		}
	}
	return nil, false
}

// Latest is the newest reviewed version this Rubi can run.
func (e *CatalogEntry) Latest(current string) (CatalogVersion, bool) {
	var best CatalogVersion
	found := false
	for _, v := range e.Versions {
		if !update.ValidVersion(v.Version) || !update.Compatible(current, v.MinRubi) {
			continue
		}
		if !found || update.Newer(v.Version, best.Version) {
			best, found = v, true
		}
	}
	return best, found
}

// Version finds a reviewed version.
func (e *CatalogEntry) Version(v string) (CatalogVersion, bool) {
	for _, cv := range e.Versions {
		if cv.Version == v {
			return cv, true
		}
	}
	return CatalogVersion{}, false
}
