package tools

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 progressive tool disclosure (#506): a tiny, dependency-free in-process
// keyword index over tool metadata. When the tool roster would blow the
// provider's 128-tool ceiling, most tools are deferred behind the
// tool_search → tool_describe → tool_call bridges; this index ranks the
// deferred tools for tool_search. No embeddings, no vector DB, no network —
// just BM25 over {name, description} tokens, which is plenty for keyword-y
// tool names/descriptions and keeps the whole feature self-hosted and
// deterministic.

// BM25 tuning — the textbook defaults; k1 dampens term-frequency saturation, b
// controls length normalization.
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// BM25Doc is one indexed item: an opaque ID (the tool's advertised name) plus
// the text to rank on (name + description).
type BM25Doc struct {
	ID   string
	Text string
}

// BM25Index is an immutable keyword index built once per tool roster.
type BM25Index struct {
	ids      []string         // doc id by internal index
	docFreq  map[string]int   // term → number of docs containing it
	docLen   []int            // token count per doc
	termFreq []map[string]int // term → count, per doc
	avgLen   float64
	docCount int
}

// NewBM25Index builds the index over docs. An empty set yields a valid,
// empty index whose Search returns nothing.
func NewBM25Index(docs []BM25Doc) *BM25Index {
	idx := &BM25Index{
		docFreq: map[string]int{},
	}
	var total int
	for _, d := range docs {
		toks := bm25Tokenize(d.Text)
		tf := map[string]int{}
		for _, tok := range toks {
			tf[tok]++
		}
		for term := range tf {
			idx.docFreq[term]++
		}
		idx.ids = append(idx.ids, d.ID)
		idx.docLen = append(idx.docLen, len(toks))
		idx.termFreq = append(idx.termFreq, tf)
		total += len(toks)
	}
	idx.docCount = len(docs)
	if idx.docCount > 0 {
		idx.avgLen = float64(total) / float64(idx.docCount)
	}
	return idx
}

// BM25Result is one scored hit.
type BM25Result struct {
	ID    string
	Score float64
}

// Search returns up to limit doc IDs ranked by BM25 score for the query,
// highest first, excluding zero-score (no term overlap) docs. A blank query or
// empty index returns nil.
func (idx *BM25Index) Search(query string, limit int) []BM25Result {
	terms := bm25Tokenize(query)
	if len(terms) == 0 || idx.docCount == 0 {
		return nil
	}
	// Dedup query terms (a repeated query word shouldn't multiply weight).
	seen := map[string]bool{}
	uniq := terms[:0]
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			uniq = append(uniq, t)
		}
	}

	results := make([]BM25Result, 0, idx.docCount)
	for i := 0; i < idx.docCount; i++ {
		var score float64
		dl := float64(idx.docLen[i])
		for _, term := range uniq {
			tf, ok := idx.termFreq[i][term]
			if !ok {
				continue
			}
			df := idx.docFreq[term]
			// BM25 idf with the +1 guard so it's never negative.
			idf := math.Log(1 + (float64(idx.docCount)-float64(df)+0.5)/(float64(df)+0.5))
			denom := float64(tf) + bm25K1*(1-bm25B+bm25B*dl/idx.avgLen)
			score += idf * (float64(tf) * (bm25K1 + 1)) / denom
		}
		if score > 0 {
			results = append(results, BM25Result{ID: idx.ids[i], Score: score})
		}
	}
	sort.SliceStable(results, func(a, b int) bool { return results[a].Score > results[b].Score })
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results
}

// bm25Tokenize lowercases and splits on non-alphanumeric boundaries, and also
// splits snake_case/camelCase tool names into their parts (so "sendEmail" and
// "send_email" both match the query "send email").
func bm25Tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	var prev rune
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// camelCase boundary: lower→Upper starts a new token.
			if cur.Len() > 0 && unicode.IsUpper(r) && unicode.IsLower(prev) {
				flush()
			}
			cur.WriteRune(unicode.ToLower(r))
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}
