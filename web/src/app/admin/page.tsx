import { permanentRedirect } from "next/navigation";

// /admin moved under the unified settings area (fleet-unified settings pass):
// the operator surface now lives at /settings/admin with per-section routes
// (overview/users/features/providers/notifications). This route survives as a
// permanent alias so bookmarks and older account menus keep working.
export default function AdminRedirect() {
  permanentRedirect("/settings/admin");
}
