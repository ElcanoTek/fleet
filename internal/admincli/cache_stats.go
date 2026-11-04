package admincli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func printJSONTo(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return errf(5, "encode json: %v", err)
	}
	return 0
}

// CacheStatsRow is one aggregated bucket of prompt-cache telemetry from
// turn_metrics (grouped by day or user).
type CacheStatsRow struct {
	Key                 string  `json:"key"`
	Turns               int64   `json:"turns"`
	CostUSD             float64 `json:"cost_usd"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	UncachedTokens      int64   `json:"uncached_tokens"`
	CacheHitRatePct     float64 `json:"cache_hit_rate_pct"`
	Status              string  `json:"status"`
}

// CacheStatsReport is the JSON/table payload emitted by `fleet cache-stats`
// (and `fleet usage`). Designed so an operator or an agent can inspect prompt
// cache health by date or user in a single read-only command without needing a
// running server or writing raw SQL.
type CacheStatsReport struct {
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	DisabledBy string          `json:"disabled_by,omitempty"`
	WindowDays int             `json:"window_days"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Totals     CacheStatsRow   `json:"totals"`
	ByDay      []CacheStatsRow `json:"by_day,omitempty"`
	ByUser     []CacheStatsRow `json:"by_user,omitempty"`
}

// evaluateCacheRowStatus classifies a bucket or total row so broken caching
// stands out immediately ("broken", "low", "ok", or "no_data").
func evaluateCacheRowStatus(turns, promptTokens, cachedTokens int64) (hitRate float64, uncached int64, status string) {
	uncached = max(int64(0), promptTokens-cachedTokens)
	if promptTokens > 0 {
		hitRate = float64(cachedTokens) / float64(promptTokens) * 100.0
	}
	switch {
	case turns == 0 || promptTokens == 0:
		status = "no_data"
	case turns >= 3 && promptTokens >= 10_000 && cachedTokens == 0:
		status = "broken"
	case turns >= 3 && promptTokens >= 10_000 && hitRate < 20.0:
		status = "low"
	default:
		status = "ok"
	}
	return hitRate, uncached, status
}

func finalizeCacheRow(r *CacheStatsRow) {
	r.CacheHitRatePct, r.UncachedTokens, r.Status = evaluateCacheRowStatus(r.Turns, r.PromptTokens, r.CachedTokens)
}

func promptCacheDisabledEnvVar() string {
	for _, k := range []string{"FLEET_DISABLE_PROMPT_CACHE", "CHAT_DISABLE_PROMPT_CACHE", "CUTLASS_DISABLE_PROMPT_CACHE"} {
		if truthy(envOrFile(k)) {
			return k
		}
	}
	return ""
}

// cmdCacheStats implements `fleet cache-stats` (alias `fleet usage`): a fast,
// read-only report of prompt cache hit rate, cached tokens, and cache misses by
// UTC date and user over the trailing N days.
func cmdCacheStats(argv []string) int {
	return runCacheStats(argv, os.Stdout)
}

func runCacheStats(argv []string, out io.Writer) int {
	fs := flag.NewFlagSet("cache-stats", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	days := fs.Int("days", 14, "trailing window in UTC days (1..366)")
	by := fs.String("by", "all", "dimension: all|day|user")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "cache-stats takes no positional arguments (got %q)", fs.Args())
	}
	if *days < 1 || *days > 366 {
		return errf(1, "--days must be between 1 and 366 (got %d)", *days)
	}
	dim := strings.ToLower(strings.TrimSpace(*by))
	switch dim {
	case "all", "day", "user":
	default:
		return errf(1, "--by must be one of: all, day, user (got %q)", *by)
	}

	dsn, err := chatDSN(*chatURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errf(5, "open chat DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rep, err := buildCacheStatsReport(ctx, db, *days, dim, promptCacheDisabledEnvVar())
	if err != nil {
		return errf(5, "query turn_metrics: %v", err)
	}
	if *asJSON {
		return printJSONTo(out, rep)
	}
	if err := renderCacheStatsReport(out, rep, dim); err != nil {
		return errf(5, "render: %v", err)
	}
	return 0
}

func buildCacheStatsReport(ctx context.Context, db *sql.DB, days int, dim, disabledBy string) (*CacheStatsReport, error) {
	now := time.Now().UTC()
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	rep := &CacheStatsReport{
		DisabledBy: disabledBy,
		WindowDays: days,
		From:       from.Format("2006-01-02"),
		To:         now.Format("2006-01-02"),
	}

	var tot CacheStatsRow
	tot.Key = "TOTAL"
	if err := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(cost_usd), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0)
		FROM turn_metrics
		WHERE completed_at >= $1 AND completed_at < $2`,
		from.Unix(), now.Unix(),
	).Scan(&tot.Turns, &tot.CostUSD, &tot.PromptTokens, &tot.CachedTokens, &tot.CacheCreationTokens); err != nil {
		return nil, err
	}
	finalizeCacheRow(&tot)
	rep.Totals = tot

	if dim == "all" || dim == "day" {
		rows, err := queryCacheDimension(ctx, db, from.Unix(), now.Unix(), "day")
		if err != nil {
			return nil, err
		}
		rep.ByDay = rows
	}
	if dim == "all" || dim == "user" {
		rows, err := queryCacheDimension(ctx, db, from.Unix(), now.Unix(), "user")
		if err != nil {
			return nil, err
		}
		rep.ByUser = rows
	}

	rep.Status = tot.Status
	if disabledBy != "" {
		rep.Status = "disabled"
		rep.Summary = fmt.Sprintf("prompt caching is disabled via %s", disabledBy)
	} else {
		switch rep.Status {
		case "no_data":
			rep.Summary = fmt.Sprintf("no chat turns recorded in the last %d day(s)", days)
		case "broken":
			rep.Summary = fmt.Sprintf("0.0%% cache hit rate across %d turns (%d prompt tokens, 0 cached) — prompt caching appears broken", tot.Turns, tot.PromptTokens)
		case "low":
			rep.Summary = fmt.Sprintf("low cache hit rate %.1f%% across %d turns (%d cached / %d prompt tokens, %d cache misses)", tot.CacheHitRatePct, tot.Turns, tot.CachedTokens, tot.PromptTokens, tot.UncachedTokens)
		default:
			rep.Summary = fmt.Sprintf("%.1f%% cache hit rate across %d turns (%d cached / %d prompt tokens, %d cache misses, $%.2f)", tot.CacheHitRatePct, tot.Turns, tot.CachedTokens, tot.PromptTokens, tot.UncachedTokens, tot.CostUSD)
		}
	}
	return rep, nil
}

const (
	cacheQueryByDay = `
		SELECT
			to_char(date_trunc('day', to_timestamp(m.completed_at) AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS bucket_key,
			COUNT(*) AS turns,
			COALESCE(SUM(m.cost_usd), 0) AS cost_usd,
			COALESCE(SUM(m.prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(m.cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(m.cache_creation_tokens), 0) AS cache_creation_tokens
		FROM turn_metrics m
		WHERE m.completed_at >= $1 AND m.completed_at < $2
		GROUP BY 1
		ORDER BY 1 DESC`
	cacheQueryByUser = `
		SELECT
			COALESCE(NULLIF(m.user_email, ''), '(unattributed)') AS bucket_key,
			COUNT(*) AS turns,
			COALESCE(SUM(m.cost_usd), 0) AS cost_usd,
			COALESCE(SUM(m.prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(m.cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(m.cache_creation_tokens), 0) AS cache_creation_tokens
		FROM turn_metrics m
		WHERE m.completed_at >= $1 AND m.completed_at < $2
		GROUP BY 1
		ORDER BY prompt_tokens DESC, 1 ASC`
)

func queryCacheDimension(ctx context.Context, db *sql.DB, fromUnix, toUnix int64, dim string) ([]CacheStatsRow, error) {
	var q string
	switch dim {
	case "day":
		q = cacheQueryByDay
	case "user":
		q = cacheQueryByUser
	default:
		return nil, fmt.Errorf("unknown dimension %q", dim)
	}

	rows, err := db.QueryContext(ctx, q, fromUnix, toUnix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []CacheStatsRow
	for rows.Next() {
		var r CacheStatsRow
		if err := rows.Scan(&r.Key, &r.Turns, &r.CostUSD, &r.PromptTokens, &r.CachedTokens, &r.CacheCreationTokens); err != nil {
			return nil, err
		}
		finalizeCacheRow(&r)
		out = append(out, r)
	}
	return out, rows.Err()
}

func renderCacheStatsReport(w io.Writer, rep *CacheStatsReport, dim string) error {
	if _, err := fmt.Fprintf(w, "Prompt Cache Efficiency (%s → %s, %dd window)\n", rep.From, rep.To, rep.WindowDays); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Status: [%s] %s\n", strings.ToUpper(rep.Status), rep.Summary); err != nil {
		return err
	}
	if rep.Totals.Turns == 0 {
		return nil
	}

	writeSection := func(title, keyHeader string, rows []CacheStatsRow) error {
		if len(rows) == 0 {
			return nil
		}
		if _, err := fmt.Fprintf(w, "\n── %s ──\n", title); err != nil {
			return err
		}
		tableRows := make([][]string, 0, len(rows))
		for _, r := range rows {
			tableRows = append(tableRows, []string{
				r.Key,
				fmt.Sprintf("%d", r.Turns),
				fmt.Sprintf("%d", r.PromptTokens),
				fmt.Sprintf("%d", r.CachedTokens),
				fmt.Sprintf("%d", r.UncachedTokens),
				fmt.Sprintf("%.1f%%", r.CacheHitRatePct),
				fmt.Sprintf("$%.4f", r.CostUSD),
				r.Status,
			})
		}
		return renderTable(w, []string{keyHeader, "TURNS", "PROMPT_TOK", "CACHED_TOK", "MISS_TOK", "HIT_RATE", "COST_USD", "STATUS"}, tableRows)
	}

	if dim == "all" || dim == "day" {
		if err := writeSection("By Date (UTC)", "DATE", rep.ByDay); err != nil {
			return err
		}
	}
	if dim == "all" || dim == "user" {
		if err := writeSection("By User", "USER", rep.ByUser); err != nil {
			return err
		}
	}
	return nil
}

// checkPromptCache adds an advisory 7-day prompt-cache health line to
// `fleet status` so operators and agents immediately see if caching is
// disabled or broken on the box.
func checkPromptCache(r *report, flagURL string) {
	if disabledBy := promptCacheDisabledEnvVar(); disabledBy != "" {
		r.warnLine("prompt cache", fmt.Sprintf("disabled via %s", disabledBy))
		return
	}
	dsn, err := chatDSN(flagURL)
	if err != nil {
		return
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := buildCacheStatsReport(ctx, db, 7, "day", "")
	if err != nil {
		return
	}
	switch rep.Status {
	case "broken", "low":
		r.warnLine("prompt cache", rep.Summary+" (inspect: fleet cache-stats)")
	default:
		r.pass("prompt cache", rep.Summary)
	}
}
