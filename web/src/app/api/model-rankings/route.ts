import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { listLatestPerLab, loadCatalog } from "@/app/lib/openrouterModels";
import { TIER_MODELS } from "@/app/lib/modelAliases";

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
    // Exclude the pinned picker rows (variant-insensitively): a lab's
    // "newest" entry that IS the pinned model would render as a duplicate
    // row right under the pin.
    const entries = listLatestPerLab(catalog, TIER_MODELS.map((t) => t.slug));
    if (entries.length === 0) {
      throw new Error("no per-lab models were found");
    }
    return NextResponse.json({
      // Prices ride along so the browse-mode rows (shown before the larger
      // catalog fetch lands, and the only source for them under a failing
      // catalog) can render the "$ … $$$$" cost indicator too.
      models: entries.map((e) => ({
        slug: e.slug,
        name: e.name,
        created: e.created,
        price_prompt: e.promptPerToken,
        price_completion: e.completionPerToken,
      })),
      cached_at: catalog.fetchedAt,
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to load model rankings." },
      { status: 502 },
    );
  }
}
