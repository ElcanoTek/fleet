import { NotFound } from "@/app/shared/ui/NotFound";
import { GUIDES, findGuide, readGuide } from "../guideContent";
import { extractHeadings } from "../headings";
import { GuideMarkdown } from "../ui/GuideMarkdown";
import { GuideNav } from "../ui/GuideNav";

// One guide, rendered. The Markdown is bundled (guideContent.ts), and only the
// two slugs in GUIDES resolve — /help/anything-else is a 404 from the server
// (asserted in guides.spec.ts) and NotFound here. No request-supplied string
// is ever joined onto a path.
// The page title ("<Guide> guide — <app>") is set by the app shell (App.tsx)
// and rendered by the server for a full load.
export default function GuidePage({ slug }: { slug: string }) {
  const guide = findGuide(slug);
  if (!guide) return <NotFound />;

  const content = readGuide(guide);
  const headings = extractHeadings(content);

  return (
    <>
      <GuideNav guides={GUIDES} headings={headings} />
      <article className="w-full min-w-0 max-w-[46rem] flex-1 motion-safe:animate-set-fade">
        <header className="mb-7">
          <h1 className="font-heading text-[1.6rem] font-bold leading-tight text-[var(--color-text-primary)]">
            {guide.title}
          </h1>
          <p className="mt-2 text-[0.92rem] leading-[1.6] text-[var(--color-text-secondary)]">
            {guide.blurb}
          </p>
        </header>
        <GuideMarkdown content={content} />
      </article>
    </>
  );
}
