import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { MODELS_PAGE_URL, validateSlug } from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

// GET /api/model-check?slug=<openrouter slug>
//
// Returns a JSON body the UI can render directly next to the custom-slug
// input. Network/catalog failures return 502 with `{ error }` so the UI can
// fail open (unknown state) without mistakenly blocking send.
export async function GET(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const slug = (request.nextUrl.searchParams.get("slug") ?? "").trim();
  if (!slug) {
    return NextResponse.json({
      allowed: false,
      slug,
      reason: "empty",
      message: "Model slug is required.",
      models_url: MODELS_PAGE_URL,
    });
  }

  try {
    const result = await validateSlug(slug);
    if (result.ok) {
      return NextResponse.json({
        allowed: true,
        slug,
        known: result.entry !== undefined,
        completion_usd_per_million:
          result.entry !== undefined ? result.entry.completionPerToken * 1_000_000 : null,
      });
    }
    return NextResponse.json({
      allowed: false,
      slug,
      reason: result.reason,
      message: result.message,
      models_url: result.modelsUrl,
      completion_usd_per_million:
        result.entry !== undefined ? result.entry.completionPerToken * 1_000_000 : null,
    });
  } catch (error) {
    return NextResponse.json(
      { error: error instanceof Error ? error.message : "Failed to validate model." },
      { status: 502 },
    );
  }
}
