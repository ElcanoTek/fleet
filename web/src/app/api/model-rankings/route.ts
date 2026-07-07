import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { listLatestPerLab, loadCatalog } from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

// GET /api/model-rankings
//
// Returns one model per major lab — the newest text-only entry from
// each (no price ceiling) — to populate the picker dropdown when no
// search query is active. The two pinned slugs are NOT excluded: a
// user typing "claude" should be able to find the pinned Claude under
// Anthropic even though it's also pinned at the top. Both rows select
// the same model.
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
      throw new Error("no per-lab models were found");
    }
    return NextResponse.json({
      models: entries.map((e) => ({ slug: e.slug, name: e.name, created: e.created })),
      cached_at: catalog.fetchedAt,
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to load model rankings." },
      { status: 502 },
    );
  }
}
