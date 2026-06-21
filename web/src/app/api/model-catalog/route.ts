import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import {
  listAllowed,
  loadCatalog,
  MAX_COMPLETION_USD_PER_MILLION,
} from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

// GET /api/model-catalog
//
// Returns the full list of OpenRouter models the chat picker is allowed
// to offer: within the completion-price ceiling and capable of text
// output. The UI searches against this list when the user types a custom
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
    }));
    return NextResponse.json({
      models,
      cached_at: catalog.fetchedAt,
      max_completion_usd_per_million: MAX_COMPLETION_USD_PER_MILLION,
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to load model catalog." },
      { status: 502 },
    );
  }
}
