package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Temporal knowledge-graph memory (#523 — the deferred "Later" stage of #515).
//
// The graph is DERIVED, PROVENANCE-LINKED data over the memories table:
// memories stay the single source of truth, every relation row links to the
// memory it was extracted from, and ALL temporal semantics derive through
// that join. The relations table carries NO time columns of its own — a
// second bi-temporal surface would drift from the first (ADR-0019's time-axis
// discipline; ADR-0029). Consequences the tests pin:
//
//   - Retiring a memory retires its relations from every as-of view for free
//     (the join filters on retired_at) — no cascade bookkeeping to get wrong.
//   - Deleting a memory deletes its relations (FK ON DELETE CASCADE).
//   - Re-extracting a memory is idempotent: ReplaceRelationsForMemory swaps
//     the memory's edges in one transaction.

// maxPredicateRunes bounds a relation's predicate — a short verb phrase, not
// a sentence. Over-long model output is truncated, not rejected.
const maxPredicateRunes = 64

// maxObjectValueRunes bounds a literal object value.
const maxObjectValueRunes = 300

// memoryEntityTypes is the closed set of entity types. Unknown values
// normalize to "other" (the same posture as memory kinds: an over-creative
// model can never poison the column).
var memoryEntityTypes = map[string]bool{
	"person":       true,
	"organization": true,
	"place":        true,
	"project":      true,
	"tool":         true,
	"topic":        true,
	"other":        true,
}

// NormalizeEntityType maps any input onto the closed entity-type set (""
// and unknown values become "other"). Exported so the HTTP layer and the
// extractor mapping share one normalization.
func NormalizeEntityType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if !memoryEntityTypes[t] {
		return "other"
	}
	return t
}

// normalizeEntityName is the match key for entity dedup: lower + trimmed +
// inner whitespace collapsed, so "Elcano  Corp" and "elcano corp" are one node.
func normalizeEntityName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// normalizePredicate lower/trims a predicate and truncates it to
// maxPredicateRunes ("works_at", "prefers", "is based in").
func normalizePredicate(p string) string {
	p = strings.ToLower(strings.Join(strings.Fields(p), " "))
	if r := []rune(p); len(r) > maxPredicateRunes {
		p = string(r[:maxPredicateRunes])
	}
	return strings.TrimSpace(p)
}

// MemoryEntity is one graph node: a named thing memories talk about.
type MemoryEntity struct {
	ID        string `json:"id"`
	UserEmail string `json:"-"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	CreatedAt int64  `json:"created_at"`
}

// GraphRelation is one graph edge as returned by GraphAsOf: the stored triple
// plus the temporal/provenance fields JOINed from its source memory. The
// Learned/Valid fields are the MEMORY's — relations own no time of their own.
type GraphRelation struct {
	ID              string
	MemoryID        string
	SubjectEntityID string
	Predicate       string
	// Exactly one of ObjectEntityID / ObjectValue is non-empty (DB CHECK).
	ObjectEntityID string
	ObjectValue    string
	MemoryContent  string
	LearnedAt      int64
	ValidFrom      *int64
	ValidTo        *int64
}

// Graph is the as-of view: every relation visible at the queried coordinates
// plus exactly the entities those relations reference.
type Graph struct {
	Entities  []MemoryEntity
	Relations []GraphRelation
}

// GraphQuery selects the temporal/scoping coordinates for GraphAsOf and
// ListMemoriesAsOf. The two axes are deliberately distinct (ADR-0019):
//
//   - AsOfLearned (TRANSACTION time): "what did fleet know/trust then?".
//     nil = NOW — the transaction axis always applies, because a fact fleet
//     has not yet learned (or has retired) is never part of the current view.
//   - AsOfValid (VALID time): "what was true in the world then?".
//     nil = NO filter — validity windows are optional user annotations, so
//     the default view includes facts regardless of their window.
//
// ProjectID nil = personal memories (project_id IS NULL); set = that
// project's shared memories. Proposals (source='proposed') are always
// excluded — an unreviewed candidate is not knowledge.
type GraphQuery struct {
	AsOfValid   *time.Time
	AsOfLearned *time.Time
	ProjectID   *string
}

// memoryAsOfWhere is the shared WHERE fragment implementing GraphQuery over a
// memories row aliased m. Fixed parameter slots (both callers use them):
// $1 = user_email, $2 = project id (nil = personal), $3 = learned-axis unix
// seconds, $4 = valid-axis unix seconds (nil = no valid filter). Kept as one
// string so GraphAsOf and ListMemoriesAsOf cannot drift.
const memoryAsOfWhere = `
	m.source != 'proposed'
	AND (CASE WHEN $2::text IS NULL THEN m.project_id IS NULL ELSE m.project_id = $2 END)
	AND COALESCE(m.learned_at, m.created_at) <= $3
	AND (m.retired_at IS NULL OR m.retired_at > $3)
	AND ($4::bigint IS NULL
		OR ((m.valid_from IS NULL OR m.valid_from <= $4)
		AND (m.valid_to IS NULL OR m.valid_to > $4)))`

// asOfParams resolves a GraphQuery to the (project, learned, valid) SQL
// parameters. learned defaults to now; valid stays nil (no filter).
func asOfParams(q GraphQuery) (project *string, learned int64, valid *int64) {
	project = q.ProjectID
	learned = time.Now().Unix()
	if q.AsOfLearned != nil {
		learned = q.AsOfLearned.Unix()
	}
	if q.AsOfValid != nil {
		v := q.AsOfValid.Unix()
		valid = &v
	}
	return project, learned, valid
}

// UpsertEntity creates or finds the (user, name, type) entity node. The match
// key is the normalized name + normalized type; name keeps the latest display
// casing. Returns the stored entity.
func (s *Store) UpsertEntity(ctx context.Context, userEmail, name, entityType string) (*MemoryEntity, error) {
	return upsertEntity(ctx, s.db, normalizeEmail(userEmail), name, entityType)
}

// execer is the subset of *sql.DB / *sql.Tx upsertEntity needs, so the
// transactional ReplaceRelationsForMemory path shares the exact upsert.
type execer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func upsertEntity(ctx context.Context, db execer, userEmail, name, entityType string) (*MemoryEntity, error) {
	name = strings.Join(strings.Fields(name), " ")
	norm := normalizeEntityName(name)
	if norm == "" {
		return nil, errors.New("entity name required")
	}
	entityType = NormalizeEntityType(entityType)
	e := &MemoryEntity{ID: uuid.NewString(), UserEmail: userEmail, Name: name, Type: entityType}
	// DO UPDATE (not DO NOTHING) so the row is always RETURNed and the display
	// name follows the freshest extraction.
	row := db.QueryRowContext(ctx,
		`INSERT INTO memory_entities (id, user_email, name, name_norm, entity_type, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (user_email, name_norm, entity_type)
		 DO UPDATE SET name = EXCLUDED.name
		 RETURNING id, name, entity_type, created_at`,
		e.ID, userEmail, name, norm, entityType, time.Now().Unix(),
	)
	if err := row.Scan(&e.ID, &e.Name, &e.Type, &e.CreatedAt); err != nil {
		return nil, err
	}
	return e, nil
}

// GraphExtraction is the store-level shape of one memory's extracted graph
// fragment (the agent package has a mirror type; the HTTP layer maps between
// them so store never imports agent). Relations reference entities by NAME:
// a relation Object matching a declared entity name links entity→entity;
// anything else is stored as a literal object_value. A Subject not declared
// under Entities is upserted as type "other" rather than dropped.
type GraphExtraction struct {
	Entities  []GraphEntityInput
	Relations []GraphRelationInput
}

// GraphEntityInput declares one entity by display name + (closed-set) type.
type GraphEntityInput struct {
	Name string
	Type string
}

// GraphRelationInput is one (subject, predicate, object) triple by name.
type GraphRelationInput struct {
	Subject   string
	Predicate string
	Object    string
}

// ReplaceRelationsForMemory swaps a memory's derived graph fragment for the
// given extraction, in ONE transaction (delete + insert), so re-extraction is
// idempotent: running the extractor twice — or correcting a bad extraction —
// never duplicates edges. Entities are upserted, never deleted (they are
// shared across memories; an orphaned node simply stops appearing in as-of
// views, which return only referenced entities). Returns the number of
// relations stored.
//
// The memory must exist and belong to userEmail — the relation rows inherit
// that scoping, and a graph row must never outlive or outreach its source.
func (s *Store) ReplaceRelationsForMemory(ctx context.Context, userEmail, memoryID string, g GraphExtraction) (int, error) {
	userEmail = normalizeEmail(userEmail)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM memories WHERE id = $1 AND user_email = $2`,
		memoryID, userEmail,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("memory not found")
	}
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_relations WHERE memory_id = $1 AND user_email = $2`,
		memoryID, userEmail,
	); err != nil {
		return 0, err
	}

	// Upsert declared entities; the norm→id map is what relations resolve
	// against. Declared type wins over the implicit-"other" fallback below.
	ids := map[string]string{}
	for _, in := range g.Entities {
		norm := normalizeEntityName(in.Name)
		if norm == "" {
			continue
		}
		e, err := upsertEntity(ctx, tx, userEmail, in.Name, in.Type)
		if err != nil {
			return 0, err
		}
		ids[norm] = e.ID
	}

	now := time.Now().Unix()
	inserted := 0
	seen := map[string]bool{} // dedup identical triples within one extraction
	for _, r := range g.Relations {
		subjNorm := normalizeEntityName(r.Subject)
		pred := normalizePredicate(r.Predicate)
		obj := strings.Join(strings.Fields(r.Object), " ")
		if subjNorm == "" || pred == "" || obj == "" {
			continue
		}
		subjID, ok := ids[subjNorm]
		if !ok {
			// Undeclared subject: keep the relation, typing the node "other" —
			// dropping data over a model bookkeeping slip would be worse.
			e, err := upsertEntity(ctx, tx, userEmail, r.Subject, "other")
			if err != nil {
				return 0, err
			}
			subjID = e.ID
			ids[subjNorm] = subjID
		}
		var objEntityID, objValue *string
		if id, ok := ids[normalizeEntityName(obj)]; ok {
			objEntityID = &id
		} else {
			if rr := []rune(obj); len(rr) > maxObjectValueRunes {
				obj = string(rr[:maxObjectValueRunes])
			}
			objValue = &obj
		}
		key := subjID + "\x00" + pred + "\x00"
		if objEntityID != nil {
			key += "e:" + *objEntityID
		} else {
			key += "v:" + strings.ToLower(*objValue)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_relations (id, user_email, memory_id, subject_entity_id, predicate, object_entity_id, object_value, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			uuid.NewString(), userEmail, memoryID, subjID, pred, objEntityID, objValue, now,
		); err != nil {
			return 0, err
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// GraphAsOf returns the user's knowledge graph as of the queried coordinates:
// every relation whose SOURCE MEMORY passes the two-axis as-of filter (see
// GraphQuery), plus exactly the entities those relations reference. All
// temporal semantics come from the memories join — a retired memory's edges
// vanish from views at/after its retirement and reappear in views dated
// before it (time-travel over the transaction axis).
func (s *Store) GraphAsOf(ctx context.Context, userEmail string, q GraphQuery) (*Graph, error) {
	project, learned, valid := asOfParams(q)
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.memory_id, r.predicate,
			se.id, se.name, se.entity_type, se.created_at,
			oe.id, oe.name, oe.entity_type, oe.created_at,
			COALESCE(r.object_value, ''),
			m.content, COALESCE(m.learned_at, m.created_at), m.valid_from, m.valid_to
		 FROM memory_relations r
		 JOIN memory_entities se ON se.id = r.subject_entity_id
		 LEFT JOIN memory_entities oe ON oe.id = r.object_entity_id
		 JOIN memories m ON m.id = r.memory_id
		 WHERE r.user_email = $1 AND `+memoryAsOfWhere+`
		 ORDER BY se.name_norm ASC, r.predicate ASC, r.id ASC`,
		normalizeEmail(userEmail), project, learned, valid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	g := &Graph{Entities: []MemoryEntity{}, Relations: []GraphRelation{}}
	seenEntity := map[string]bool{}
	addEntity := func(e MemoryEntity) {
		if !seenEntity[e.ID] {
			seenEntity[e.ID] = true
			g.Entities = append(g.Entities, e)
		}
	}
	for rows.Next() {
		var (
			rel     GraphRelation
			subj    MemoryEntity
			objID   sql.NullString
			objName sql.NullString
			objType sql.NullString
			objAt   sql.NullInt64
		)
		if err := rows.Scan(&rel.ID, &rel.MemoryID, &rel.Predicate,
			&subj.ID, &subj.Name, &subj.Type, &subj.CreatedAt,
			&objID, &objName, &objType, &objAt,
			&rel.ObjectValue,
			&rel.MemoryContent, &rel.LearnedAt, &rel.ValidFrom, &rel.ValidTo); err != nil {
			return nil, err
		}
		rel.SubjectEntityID = subj.ID
		addEntity(subj)
		if objID.Valid {
			rel.ObjectEntityID = objID.String
			addEntity(MemoryEntity{ID: objID.String, Name: objName.String, Type: objType.String, CreatedAt: objAt.Int64})
		}
		g.Relations = append(g.Relations, rel)
	}
	return g, rows.Err()
}

// ListMemoriesAsOf is the flat-record twin of GraphAsOf (#523 asks for as-of
// over records, not only the graph): the user's memories as of the queried
// coordinates, with identical two-axis semantics, proposal exclusion, and
// project scoping. Ordering matches ListMemories' active section (pinned
// first, freshest next) — there is no retired trailer because a memory
// retired before the learned-axis cutoff is simply not in the view.
func (s *Store) ListMemoriesAsOf(ctx context.Context, userEmail string, q GraphQuery) ([]Memory, error) {
	project, learned, valid := asOfParams(q)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+memoryColumns+`
		 FROM memories m
		 WHERE m.user_email = $1 AND `+memoryAsOfWhere+`
		 ORDER BY m.pinned DESC, m.updated_at DESC, m.id DESC`,
		normalizeEmail(userEmail), project, learned, valid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
