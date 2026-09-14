import { notFound } from "next/navigation";
import { GUIDES, findGuide, readGuide } from "../guideContent";
import { extractHeadings } from "../headings";
import { guidePageTitle } from "../metadata";
import { GuideMarkdown } from "../ui/GuideMarkdown";
import { GuideNav } from "../ui/GuideNav";

// One guide, rendered. The root layout is force-dynamic for white-labeling, so
// this renders per request like every other page in the app — but the Markdown
// itself is read from disk once per server process (guideContent.ts memoizes
// it), so serving a guide is a render, never a file read.

// Not for prerendering (see above) — this is the allowlist `dynamicParams`
// checks a slug against, so /help/anything-else answers 404 (asserted in
// guides.spec.ts) instead of reaching the page. findGuide() below is the
// second of the two locks, and the reason no request-supplied string is ever
// joined onto a path.
export function generateStaticParams() {
  return GUIDES.map((guide) => ({ slug: guide.slug }));
}

export const dynamicParams = false;

export async function generateMetadata({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  const guide = findGuide(slug);
  return { title: await guidePageTitle(guide ? `${guide.title} guide` : "Guides") };
}

export default async function GuidePage({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  const guide = findGuide(slug);
  if (!guide) notFound();

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
