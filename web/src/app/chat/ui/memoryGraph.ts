// Pure helpers for the memory knowledge-graph view (#523) — kept free of React
// so the grouping/formatting logic is unit-testable with vitest.

export type GraphEntity = {
  id: string;
  name: string;
  type: string;
};

export type GraphRelation = {
  id: string;
  subject: GraphEntity;
  predicate: string;
  // Exactly one of entity / value is set (mirrors the backend CHECK).
  object: { entity?: GraphEntity; value?: string };
  memory_id: string;
  memory_content_snippet: string;
  learned_at: number;
  valid_from?: number;
  valid_to?: number;
};

export type GraphResponse = {
  entities: GraphEntity[];
  relations: GraphRelation[];
};

// relationObjectLabel renders the right-hand side of a triple: the target
// entity's name for an entity→entity edge, the literal otherwise.
export function relationObjectLabel(relation: GraphRelation): string {
  return relation.object.entity?.name ?? relation.object.value ?? "";
}

export type SubjectGroup = {
  subject: GraphEntity;
  relations: GraphRelation[];
};

// groupRelationsBySubject buckets relations under their subject entity,
// preserving the server's ordering (subjects alphabetical, then predicate).
export function groupRelationsBySubject(relations: GraphRelation[]): SubjectGroup[] {
  const groups = new Map<string, SubjectGroup>();
  for (const relation of relations) {
    const existing = groups.get(relation.subject.id);
    if (existing) {
      existing.relations.push(relation);
    } else {
      groups.set(relation.subject.id, { subject: relation.subject, relations: [relation] });
    }
  }
  return [...groups.values()];
}

// datetimeLocalToRFC3339 converts a datetime-local input value (interpreted in
// the browser's timezone) to the RFC3339 string the API expects. Empty or
// unparseable input → null (no filter).
export function datetimeLocalToRFC3339(value: string): string | null {
  if (!value.trim()) return null;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return null;
  return date.toISOString();
}

// buildGraphQueryString renders the two optional as-of inputs into the
// /api/memories/graph query string ("" when both are unset).
export function buildGraphQueryString(asOfValid: string, asOfLearned: string): string {
  const params = new URLSearchParams();
  const valid = datetimeLocalToRFC3339(asOfValid);
  const learned = datetimeLocalToRFC3339(asOfLearned);
  if (valid) params.set("as_of_valid", valid);
  if (learned) params.set("as_of_learned", learned);
  const qs = params.toString();
  return qs ? `?${qs}` : "";
}

// relationValiditySuffix annotates a relation whose source memory carries a
// validity window ("true 2024-01-01 → 2024-12-31", "true since …", "true until …").
export function relationValiditySuffix(relation: GraphRelation): string {
  const day = (unix: number) => new Date(unix * 1000).toISOString().slice(0, 10);
  const { valid_from: from, valid_to: to } = relation;
  if (from != null && to != null) return `true ${day(from)} → ${day(to)}`;
  if (from != null) return `true since ${day(from)}`;
  if (to != null) return `true until ${day(to)}`;
  return "";
}
