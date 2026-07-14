import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";
type Context = { params: Promise<{ promptId: string }> };

export async function PUT(request: NextRequest, context: Context) {
  const { promptId } = await context.params;
  return proxyToOrchestrator(request, `/prompts/${encodeURIComponent(promptId)}`);
}

export async function DELETE(request: NextRequest, context: Context) {
  const { promptId } = await context.params;
  return proxyToOrchestrator(request, `/prompts/${encodeURIComponent(promptId)}`);
}
