import { notFound } from "next/navigation";
import { GUIDES, findGuide, readGuide } from "../guideContent";
import { extractHeadings } from "../headings";
import { GuideMarkdown } from "../ui/GuideMarkdown";
import { GuideNav } from "../ui/GuideNav";

// One guide, rendered. Static: the Markdown is read once at build time and the
// two pages are pre-rendered, so opening a guide costs a signed-in user nothing
// but the navigation.

export function generateStaticParams() {
  return GUIDES.map((guide) => ({ slug: guide.slug }));
}

// Anything outside the GUIDES table is a 404, so the slug never reaches the
// filesystem.
export const dynamicParams = false;

export async function generateMetadata({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  const guide = findGuide(slug);
  return { title: guide ? `${guide.title} guide` : "Guides" };
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
