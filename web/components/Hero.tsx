import { headline, repo } from "@/lib/content";
import { Reveal } from "./Reveal";
import { Converge } from "./Converge";

export function Hero() {
  return (
    <header className="relative isolate overflow-hidden">
      <div className="grid-field grid-fade pointer-events-none absolute inset-0 -z-10" />
      <div className="glow pointer-events-none absolute inset-0 -z-10" />

      <nav className="mx-auto flex w-full max-w-6xl items-center justify-between px-5 py-6 sm:px-8">
        <div className="flex items-center gap-2.5">
          <span className="relative flex h-2 w-2">
            <span className="absolute inline-flex h-full w-full rounded-full bg-accent opacity-60 animate-blink" />
            <span className="relative inline-flex h-2 w-2 rounded-full bg-accent" />
          </span>
          <span className="font-mono text-sm tracking-tight">idemio</span>
        </div>
        <div className="flex items-center gap-6 text-sm">
          <a href="#how" className="hidden py-1.5 text-muted transition-colors hover:text-text sm:block">
            How it works
          </a>
          <a href="#measured" className="hidden py-1.5 text-muted transition-colors hover:text-text sm:block">
            Measured
          </a>
          <a
            href={repo}
            target="_blank"
            rel="noreferrer"
            className="rounded-full border border-line-strong px-3.5 py-1.5 transition-colors hover:border-accent hover:text-accent"
          >
            GitHub
          </a>
        </div>
      </nav>

      <div className="mx-auto w-full max-w-6xl px-5 pb-16 pt-10 sm:px-8 sm:pb-24 sm:pt-20">
        <Reveal>
          <p className="font-mono text-[11px] uppercase tracking-[0.18em] text-dim sm:text-xs">
            {headline.eyebrow}
          </p>
        </Reveal>

        <Reveal delay={80}>
          <h1 className="mt-7 text-[clamp(2.05rem,8.6vw,5.2rem)] font-semibold leading-[1.03] tracking-[-0.03em] text-balance">
            <span className="block text-muted">{headline.title[0]}</span>
            <span className="block">{headline.title[1]}</span>
          </h1>
        </Reveal>

        <Reveal delay={160}>
          <p className="mt-8 max-w-2xl text-base leading-relaxed text-muted sm:text-lg">
            {headline.sub}
          </p>
        </Reveal>

        <Reveal delay={240}>
          <div className="mt-10 flex flex-col gap-3 sm:flex-row sm:items-center">
            <a
              href={repo}
              target="_blank"
              rel="noreferrer"
              className="group inline-flex items-center justify-center gap-2 rounded-full bg-accent px-6 py-3 text-sm font-medium text-ink-950 transition-transform duration-200 hover:-translate-y-0.5"
            >
              Read the source
              <span className="transition-transform duration-200 group-hover:translate-x-0.5">
                →
              </span>
            </a>
            <a
              href="#proof"
              className="inline-flex items-center justify-center gap-2 rounded-full border border-line-strong px-6 py-3 text-sm text-muted transition-colors hover:border-line-strong hover:text-text"
            >
              See the guarantee
            </a>
          </div>
        </Reveal>

        <Reveal delay={320}>
          <div className="mt-16 sm:mt-20">
            <Converge />
          </div>
        </Reveal>
      </div>
    </header>
  );
}
