import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";

export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  return NextResponse.json({ email: session.email });
}
