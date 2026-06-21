import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import {
  listLatestPerLab,
  loadCatalog,
  MAX_COMPLETION_USD_PER_MILLION,
} from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

// GET /api/model-rankings
//
// Returns one model per major lab — the newest within-budget text-only
// entry from each — to populate the picker dropdown when no search
// query is active. Tier slugs (default/advanced) are NOT excluded: a
// user typing "claude" should be able to find Claude Sonnet listed
// under Anthropic even though it's also pinned at the top under the
// "advanced" alias. Both rows select the same model.
//
// We previously scraped openrouter.ai/rankings?view=day for this. That
// endpoint isn't officially supported and returned a daily popularity
// signal rather than what users actually want here, which is a curated
// "what's new at each major lab" cross-section. Now we derive the list
// directly from /api/v1/models via the shared catalog cache.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  try {
    const catalog = await loadCatalog();
    const entries = listLatestPerLab(catalog);
    if (entries.length === 0) {
      throw new Error("no per-lab models within budget were found");
    }
    return NextResponse.json({
      models: entries.map((e) => ({ slug: e.slug, name: e.name, created: e.created })),
      cached_at: catalog.fetchedAt,
      max_completion_usd_per_million: MAX_COMPLETION_USD_PER_MILLION,
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to load model rankings." },
      { status: 502 },
    );
  }
}
