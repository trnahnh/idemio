import { flow } from "@/lib/content";
import { Reveal } from "./Reveal";

export function Flow() {
  return (
    <ol className="relative">
      <div
        aria-hidden
        className="absolute left-[15px] top-2 bottom-2 w-px bg-gradient-to-b from-transparent via-line-strong to-transparent sm:left-[19px]"
      />

      {flow.map((s, i) => (
        <Reveal as="li" key={s.step} delay={i * 60} className="relative flex gap-5 pb-10 sm:gap-7">
          <div className="relative z-10 shrink-0">
            <div
              className={`flex h-8 w-8 items-center justify-center rounded-full border font-mono text-[10px] sm:h-10 sm:w-10 sm:text-[11px] ${
                s.accent
                  ? "border-accent/60 bg-accent-dim text-accent"
                  : "border-line-strong bg-ink-900 text-muted"
              }`}
            >
              {s.step}
            </div>
          </div>

          <div className="min-w-0 flex-1 pt-1">
            <h3 className="text-lg font-medium tracking-tight sm:text-xl">{s.title}</h3>
            <p className="mt-2 max-w-2xl text-sm leading-relaxed text-muted sm:text-[15px]">
              {s.body}
            </p>

            {s.boundary ? (
              <div className="mt-5 flex items-center gap-3 rounded-xl border border-accent/25 bg-accent-dim px-4 py-3">
                <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-accent">
                  the boundary
                </span>
                <span className="text-xs leading-relaxed text-muted">
                  Claim before execute, and never hold the transaction across the call. Both
                  halves matter, and reversing either one breaks the guarantee.
                </span>
              </div>
            ) : null}
          </div>
        </Reveal>
      ))}
    </ol>
  );
}
