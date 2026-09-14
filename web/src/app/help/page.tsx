import Link from "next/link";
import { GUIDES } from "./guideContent";
import { GuideNav } from "./ui/GuideNav";

// /help — the guides index. Two things a reader needs before either guide: the
// loop the whole product is shaped around, and which guide covers which half of
// it.

export const metadata = { title: "Guides" };

// The loop both guides open with, drawn once here: a conversation becomes a
// saved prompt, the prompt becomes a scheduled task, the task delivers itself.
const LOOP = ["conversation", "saved prompt", "scheduled task", "your inbox"];

export default function HelpIndex() {
  return (
    <>
      <GuideNav guides={GUIDES} />
      <div className="w-full min-w-0 max-w-[46rem] flex-1 motion-safe:animate-set-fade">
        <header className="mb-7">
          <h1 className="font-heading text-[1.6rem] font-bold leading-tight text-[var(--color-text-primary)]">
            Guides
          </h1>
          <p className="mt-2 text-[0.92rem] leading-[1.6] text-[var(--color-text-secondary)]">
            How this platform works, written for the people using it. Two guides,
            one for each surface.
          </p>
        </header>

        <div
          className="mb-8 flex flex-wrap items-center gap-x-2 gap-y-1.5 rounded-[var(--radius-lg)] border border-[var(--color-border)] px-4 py-3"
          role="img"
          aria-label="The loop: a conversation becomes a saved prompt, which becomes a scheduled task, which delivers to your inbox."
        >
          {LOOP.map((step, i) => (
            <span key={step} className="flex items-center gap-2">
              {i > 0 ? (
                <span aria-hidden="true" className="text-[var(--color-text-muted)]">
                  →
                </span>
              ) : null}
              <span className="font-[family-name:var(--font-code)] text-[0.78rem] text-[var(--color-text-secondary)]">
                {step}
              </span>
            </span>
          ))}
        </div>

        <div className="grid gap-3">
          {GUIDES.map((guide) => (
            <Link
              key={guide.slug}
              href={`/help/${guide.slug}`}
              data-testid={`guide-card-${guide.slug}`}
              className="group rounded-[var(--radius-lg)] border border-[var(--color-border)] px-5 py-4 no-underline transition hover:border-[var(--color-border-strong)] hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              <span className="flex items-center gap-2 font-heading text-[1.02rem] font-semibold text-[var(--color-text-primary)]">
                {guide.title}
                <span
                  aria-hidden="true"
                  className="text-[var(--color-text-muted)] transition group-hover:translate-x-0.5"
                >
                  →
                </span>
              </span>
              <span className="mt-1.5 block text-[0.85rem] leading-[1.55] text-[var(--color-text-secondary)]">
                {guide.blurb}
              </span>
            </Link>
          ))}
        </div>

        {/* The guides are also the `fleet-guide` skill, so the assistant answers
            from this same text. Saying so is the difference between a user
            hunting through a page and just asking. */}
        <p className="mt-8 rounded-[var(--radius-lg)] border border-[var(--color-border)] px-5 py-4 text-[0.85rem] leading-[1.6] text-[var(--color-text-secondary)]">
          <strong className="font-semibold text-[var(--color-text-primary)]">
            You can also just ask.
          </strong>{" "}
          The assistant reads these same guides, so &ldquo;how do I make this run
          every Monday?&rdquo; is a fair question to put to it in{" "}
          <Link href="/chat" className="underline underline-offset-2">
            Chat
          </Link>
          . It will tell you where the control is — and often offer to do it.
        </p>
      </div>
    </>
  );
}
