package httpapi

import (
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/store"
)

// catalogDrift names what the shipped directory entry of a saved connection's
// name now says differently from the row (#1006 catalog audit, F13).
//
// A row keeps the URL and auth shape it was added with. The directory is a
// snapshot a release corrects (Cartesia's and Octagon's endpoints moved,
// Composio's auth shape changed), never the connection's owner, and a user
// may have typed a URL of their own under a directory name — so a correction
// is SURFACED on the row, not applied to it: the settings page badges the row
// and says what the directory lists now, and the user removes and re-adds the
// connection to take it. There is nothing to carry over silently: an OAuth
// login is bound to the old resource, an api key was sealed against the old
// header, and a changed auth kind is a different form altogether.
//
// Only the fields that differ are set, each to the directory's current
// value. A nil drift means the row matches its entry, the name is not a
// directory entry (a hand-typed server), or the entry is a tenant one (its
// URL is a template the user filled in and its login is per-tenant). An
// entry whose URL carries a {placeholder} but authenticates by key or not at
// all (Composio, Scrapfly, thirdweb) still has its auth compared — Composio's
// Phase 4 reshaping from a tenant login to an api key is exactly the case —
// only its URL is not.
//
// The join is the name, and a name is not provenance: a server the user
// typed by hand under a directory name is compared like one added from the
// directory, and shows the same badge when it differs. The page's wording
// says so rather than claim to know where a row came from.
type catalogDrift struct {
	// URL is the directory's current endpoint, as the entry spells it.
	URL string `json:"url,omitempty"`
	// Auth is the directory's current auth kind (oauth, api_key, open).
	Auth string `json:"auth,omitempty"`
	// KeySentAs is where the directory now says an api key goes ("the
	// Authorization: Bearer header", "the X-Api-Key header", "the api_key
	// query parameter"); set only when both sides are api_key connections.
	KeySentAs string `json:"key_sent_as,omitempty"`
}

// remoteServerView is a saved connection on the wire: the row plus the
// directory drift, when there is one.
type remoteServerView struct {
	store.RemoteMCPServer
	CatalogDrift *catalogDrift `json:"catalog_drift,omitempty"`
}

// driftFromCatalog compares one saved row with the directory entry of its
// name. The directory add form saves the entry's name as the row's name, so
// the name is the join; a hand-typed server under another name matches
// nothing and reports nothing.
func driftFromCatalog(row store.RemoteMCPServer, catalog []clientconfig.RemoteMCPCatalogEntry) *catalogDrift {
	var entry *clientconfig.RemoteMCPCatalogEntry
	for i := range catalog {
		if catalog[i].Name == row.Name {
			entry = &catalog[i]
			break
		}
	}
	if entry == nil || entry.Auth == "tenant" {
		return nil
	}
	entryAuth := entry.Auth
	if entryAuth == "" {
		entryAuth = store.RemoteMCPAuthOAuth
	}
	rowAuth := row.AuthKind
	if rowAuth == "" { // rows from before the column are OAuth connections
		rowAuth = store.RemoteMCPAuthOAuth
	}
	var d catalogDrift
	if entryAuth != rowAuth {
		d.Auth = entryAuth
	}
	// The row holds the canonical form of what was typed (default port
	// dropped, host lower-cased, a lone "/" removed), so the entry is
	// canonicalised the same way before the two are compared. A URL with a
	// {placeholder} is a template the user filled in, not an address, and an
	// entry that does not canonicalise is not comparable: neither reports
	// URL drift.
	if !strings.Contains(entry.URL, "{") {
		if canon, err := mcpoauth.CanonicalResourceURI(entry.URL); err == nil && canon != row.URL {
			d.URL = entry.URL
		}
	}
	if entryAuth == store.RemoteMCPAuthAPIKey && rowAuth == store.RemoteMCPAuthAPIKey &&
		keyPlacementKey(entry.APIKeyHeader, entry.APIKeyQuery) != keyPlacementKey(row.APIKeyHeader, row.APIKeyQuery) {
		d.KeySentAs = keyPlacement(entry.APIKeyHeader, entry.APIKeyQuery)
	}
	if d == (catalogDrift{}) {
		return nil
	}
	return &d
}

// keyPlacementKey is the comparable identity of where an api key is sent,
// from the header or query-parameter NAME the directory or the row records
// ("" for both = the Authorization: Bearer header). A header name is
// case-insensitive on the wire and the service stores it as typed, so it is
// folded; a query-parameter name is case-sensitive and compared exactly.
func keyPlacementKey(header, query string) string {
	switch {
	case query != "":
		return "query:" + query
	case header != "":
		return "header:" + strings.ToLower(header)
	default:
		return "bearer"
	}
}

// keyPlacement words a key placement for the page's sub line.
func keyPlacement(header, query string) string {
	switch {
	case query != "":
		return "the " + query + " query parameter"
	case header != "":
		return "the " + header + " header"
	default:
		return "the Authorization: Bearer header"
	}
}
