import { NextResponse } from "next/server";
import { BUILD_ID_HEADER, currentBuildId } from "@/app/lib/buildId";

export const runtime = "nodejs";
// Don't cache — the whole point is a fresh probe against the live
// server's build id. `dynamic = "force-dynamic"` belt-and-suspenders
// against Next's default static optimization.
export const dynamic = "force-dynamic";

/**
 * GET /api/version
 *
 * Tiny probe. Returns the current build id both in the body (so the
 * client can read JSON) and in the X-App-Version header (so generic
 * fetch interceptors can compare without parsing). Used by the client
 * to decide whether to trigger a silent reload after a deploy.
 */
export async function GET() {
  const id = currentBuildId();
  return new NextResponse(JSON.stringify({ buildId: id }), {
    status: 200,
    headers: {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
      [BUILD_ID_HEADER]: id,
    },
  });
}
