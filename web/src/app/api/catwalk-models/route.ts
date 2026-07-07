import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { loadCatwalkProviders } from "@/app/lib/catwalkModels";

export const runtime = "nodejs";

// GET /api/catwalk-models
//
// Session-gated proxy for the catwalk model database (the no-auth catalog
// Charm's Crush uses). The pickers call this to expand an admin-configured
// catch-all workspace provider (empty models list) into a browsable model
// catalog by provider type — the direct provider APIs can't enumerate models
// without an API key, and the browser can't reach catwalk itself (CSP pins
// connect-src to 'self'). Advisory: failures return 502 and the pickers
// degrade to free-typed slugs.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  try {
    const providers = await loadCatwalkProviders();
    return NextResponse.json({
      providers: providers.map((p) => ({
        id: p.id,
        name: p.name,
        type: p.type,
        models: p.models.map((m) => ({
          id: m.id,
          name: m.name,
          context_window: m.contextWindow,
          cost_per_1m_out: m.costPer1MOut,
        })),
      })),
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to load catwalk catalog." },
      { status: 502 },
    );
  }
}
