import { clsx, type ClassValue } from "clsx"
import { extendTailwindMerge } from "tailwind-merge"

// Flag's type scale (text-title, text-caption, ...) comes from the flag-theme
// `@theme inline` block. Tell tailwind-merge they are font sizes, or it reads
// them as colors and drops one of `text-caption text-subtle`.
const twMerge = extendTailwindMerge({
  extend: {
    theme: {
      text: ["title", "subtitle", "body", "body-md", "caption", "overline"],
    },
  },
})

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
