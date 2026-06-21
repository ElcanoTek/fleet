import type { MetadataRoute } from "next";

export default function manifest(): MetadataRoute.Manifest {
  return {
    name: "Elcano Chat",
    short_name: "Elcano Chat",
    description:
      "Persistent multi-turn conversations with real tool use across email, files, and analytics.",
    start_url: "/",
    scope: "/",
    display: "standalone",
    background_color: "#1a0b1e",
    theme_color: "#1a0b1e",
    icons: [
      {
        src: "/app-icons/icon-192.png",
        sizes: "192x192",
        type: "image/png",
        purpose: "any",
      },
      {
        src: "/app-icons/icon-512.png",
        sizes: "512x512",
        type: "image/png",
        purpose: "any",
      },
      {
        src: "/app-icons/maskable-icon-512.png",
        sizes: "512x512",
        type: "image/png",
        purpose: "maskable",
      },
    ],
  };
}
