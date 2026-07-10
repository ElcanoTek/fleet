import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { listAllowed, loadCatalog } from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

// GET /api/model-catalog
//
// Returns the full list of OpenRouter models the chat picker offers:
// any catalog model capable of text output (no price ceiling). The UI searches against this list when the user types a custom
// slug so they can discover a cheaper model without leaving the page.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  try {
    const catalog = await loadCatalog();
    const models = listAllowed(catalog).map((entry) => ({
      slug: entry.slug,
      name: entry.name,
      context_length: entry.contextLength,
      created: entry.created,
      // Completion price per token — the task-form picker uses it as a
      // quality-proxy tiebreak when ranking search matches.
      price_completion: entry.completionPerToken,
    }));
    return NextResponse.json({
      models,
      cached_at: catalog.fetchedAt,
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to load model catalog." },
      { status: 502 },
    );
  }
}
